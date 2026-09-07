package localdolt

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kninetimmy/memdolt/internal/memory"
	"github.com/kninetimmy/memdolt/internal/store"
)

type FactAddOptions struct {
	Fact   Fact         `json:"fact"`
	Source string       `json:"source"`
	Actor  memory.Actor `json:"actor"`
}

type DecisionAddOptions struct {
	Decision Decision     `json:"decision"`
	Source   string       `json:"source"`
	Actor    memory.Actor `json:"actor"`
}

// HumanMemoryResult carries a confirmed identity/hash even when finalization
// fails. Unchanged means no row changed and no empty commit was manufactured.
type HumanMemoryResult struct {
	Kind   string `json:"kind"`
	ID     string `json:"id"`
	Status string `json:"status"`
	By     string `json:"by,omitempty"`
	Commit string `json:"commit,omitempty"`
	Error  string `json:"error,omitempty"`
}

// FactAdd is an explicit human assertion, separate from proposal staging. Only
// the live row is updated; older rows and their Dolt history remain intact.
func (s *Store) FactAdd(ctx context.Context, opts FactAddOptions) (HumanMemoryResult, error) {
	f := opts.Fact
	if err := f.validate(); err != nil {
		return HumanMemoryResult{}, err
	}
	if !validDistinctDottedKey(f.Key, "") {
		return HumanMemoryResult{}, errors.New("fact add requires a dotted key with no empty segment or surrounding whitespace")
	}
	if err := humanSource(opts.Source); err != nil {
		return HumanMemoryResult{}, err
	}
	f.Kind = normalizedOptional(f.Kind)
	for _, field := range []struct {
		name, value string
		chars       int
	}{{"fact key", f.Key, 255}, {"fact value", f.Value, 0}, {"fact kind", f.Kind, 64}, {"fact evidence", f.Evidence, 1024}} {
		if err := humanText(field.name, field.value, field.chars); err != nil {
			return HumanMemoryResult{}, err
		}
	}
	return s.humanMemoryWrite(ctx, opts.Actor, append(f.text(), opts.Source), "fact add", func(conn *sql.Conn) (HumanMemoryResult, []store.Statement, error) {
		now := time.Now().UTC().Truncate(time.Second)
		result := HumanMemoryResult{Kind: "fact", ID: newID(), Status: "created"}
		var matches int
		if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM facts WHERE live_key = ?", f.Key).Scan(&matches); err != nil {
			return result, nil, err
		}
		if matches > 1 {
			return result, nil, errors.New("ambiguous live fact key; repair the live-key uniqueness violation before adding")
		}
		var id, key string
		err := conn.QueryRowContext(ctx, "SELECT id, `key` FROM facts WHERE live_key = ?", f.Key).Scan(&id, &key)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return result, nil, fmt.Errorf("resolve live fact key: %w", err)
		}
		if err == nil {
			if key != f.Key {
				return result, nil, errors.New("fact key collides with a different spelling under the database collation; use the stored key")
			}
			var value, source, kind, evidence sql.NullString
			var verified sql.NullTime
			if err := conn.QueryRowContext(ctx, "SELECT value, source, kind, evidence, verified_at FROM facts WHERE id = ?", id).
				Scan(&value, &source, &kind, &evidence, &verified); err != nil {
				return result, nil, err
			}
			result.ID, result.Status = id, "updated"
			if value.Valid && value.String == f.Value && source.Valid && source.String == opts.Source &&
				equalStringPointers(nullStringPointer(kind), optionalPointer(f.Kind)) && equalStringPointers(nullStringPointer(evidence), optionalPointer(f.Evidence)) &&
				verified.Valid && verified.Time.Equal(now) {
				result.Status = "unchanged"
				return result, nil, nil
			}
			return result, []store.Statement{{
				SQL:  "UPDATE facts SET value = ?, source = ?, kind = ?, evidence = ?, verified_at = ? WHERE id = ?",
				Args: []any{f.Value, opts.Source, nullable(f.Kind), nullable(f.Evidence), now, id},
			}}, nil
		}
		return result, []store.Statement{{
			SQL:  "INSERT INTO facts (id, `key`, value, source, kind, evidence, verified_at, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
			Args: []any{result.ID, f.Key, f.Value, opts.Source, nullable(f.Kind), nullable(f.Evidence), now, now},
		}}, nil
	})
}

func (s *Store) FactVerify(ctx context.Context, ident string, actor memory.Actor) (HumanMemoryResult, error) {
	return s.humanMemoryWrite(ctx, actor, nil, "fact verify", func(conn *sql.Conn) (HumanMemoryResult, []store.Statement, error) {
		id, err := resolveHumanFact(ctx, conn, ident)
		result := HumanMemoryResult{Kind: "fact", ID: id, Status: "verified"}
		if err != nil {
			return result, nil, err
		}
		var verified sql.NullTime
		if err := conn.QueryRowContext(ctx, "SELECT verified_at FROM facts WHERE id = ?", id).Scan(&verified); err != nil {
			return result, nil, err
		}
		now := time.Now().UTC().Truncate(time.Second)
		if verified.Valid && verified.Time.Equal(now) {
			result.Status = "unchanged"
			return result, nil, nil
		}
		return result, []store.Statement{{SQL: "UPDATE facts SET verified_at = ? WHERE id = ?", Args: []any{now, id}}}, nil
	})
}

func (s *Store) DecisionAdd(ctx context.Context, opts DecisionAddOptions) (HumanMemoryResult, error) {
	d := opts.Decision
	if err := d.validate(); err != nil {
		return HumanMemoryResult{}, err
	}
	if err := humanSource(opts.Source); err != nil {
		return HumanMemoryResult{}, err
	}
	d.Summary = normalizedOptional(d.Summary)
	for _, field := range []struct {
		name, value string
		chars       int
	}{{"decision title", d.Title, 512}, {"decision rationale", d.Rationale, 0}, {"decision summary", d.Summary, 0},
		{"decision alternatives", d.AlternativesRejected, 0}, {"decision evidence", d.Evidence, 1024}} {
		if err := humanText(field.name, field.value, field.chars); err != nil {
			return HumanMemoryResult{}, err
		}
	}
	return s.humanMemoryWrite(ctx, opts.Actor, append(d.text(), opts.Source), "decision add", func(_ *sql.Conn) (HumanMemoryResult, []store.Statement, error) {
		result := HumanMemoryResult{Kind: "decision", ID: newID(), Status: "created"}
		return result, []store.Statement{{
			SQL:  "INSERT INTO decisions (id, title, rationale, summary, alternatives_rejected, evidence, status, source, decided_at) VALUES (?, ?, ?, ?, ?, ?, 'active', ?, ?)",
			Args: []any{result.ID, d.Title, d.Rationale, nullable(d.Summary), nullable(d.AlternativesRejected), nullable(d.Evidence), opts.Source, time.Now().UTC().Truncate(time.Second)},
		}}, nil
	})
}

func (s *Store) DecisionSetSummary(ctx context.Context, id, summary string, actor memory.Actor) (HumanMemoryResult, error) {
	summary = normalizedOptional(summary)
	if err := humanText("decision summary", summary, 0); err != nil {
		return HumanMemoryResult{}, err
	}
	return s.humanMemoryWrite(ctx, actor, []string{summary}, "decision set-summary", func(conn *sql.Conn) (HumanMemoryResult, []store.Statement, error) {
		result := HumanMemoryResult{Kind: "decision", ID: id, Status: "updated"}
		var old sql.NullString
		if err := conn.QueryRowContext(ctx, "SELECT id, summary FROM decisions WHERE id = ?", id).Scan(&result.ID, &old); err != nil {
			return result, nil, fmt.Errorf("resolve decision id: %w", err)
		}
		if equalStringPointers(nullStringPointer(old), optionalPointer(summary)) {
			result.Status = "unchanged"
			return result, nil, nil
		}
		return result, []store.Statement{{SQL: "UPDATE decisions SET summary = ? WHERE id = ?", Args: []any{nullable(summary), result.ID}}}, nil
	})
}

func (s *Store) FactSupersede(ctx context.Context, old, by string, actor memory.Actor) (HumanMemoryResult, error) {
	return s.humanSupersede(ctx, "fact", old, by, actor)
}

func (s *Store) DecisionSupersede(ctx context.Context, old, by string, actor memory.Actor) (HumanMemoryResult, error) {
	return s.humanSupersede(ctx, "decision", old, by, actor)
}

func (s *Store) humanSupersede(ctx context.Context, kind, old, by string, actor memory.Actor) (HumanMemoryResult, error) {
	return s.humanMemoryWrite(ctx, actor, nil, kind+" supersede", func(conn *sql.Conn) (HumanMemoryResult, []store.Statement, error) {
		var err error
		table := "decisions"
		if kind == "fact" {
			table = "facts"
			old, err = resolveHumanFact(ctx, conn, old)
			if err == nil {
				by, err = resolveHumanFact(ctx, conn, by)
			}
		}
		result := HumanMemoryResult{Kind: kind, ID: old, By: by, Status: "superseded"}
		if err != nil {
			return result, nil, err
		}
		var prior sql.NullString
		if err := conn.QueryRowContext(ctx, "SELECT id, superseded_by FROM "+table+" WHERE id = ?", old).Scan(&result.ID, &prior); err != nil {
			return result, nil, fmt.Errorf("resolve old %s: %w", kind, err)
		}
		old = result.ID
		seen := map[string]bool{old: true}
		first := true
		for next := by; ; {
			var canonical string
			var link sql.NullString
			if err := conn.QueryRowContext(ctx, "SELECT id, superseded_by FROM "+table+" WHERE id = ?", next).Scan(&canonical, &link); err != nil {
				return result, nil, fmt.Errorf("resolve replacement %s chain (no dangling links allowed): %w", kind, err)
			}
			// Compare stored identities, not operand spellings: SQL collation
			// can resolve a lowercase ULID to the same uppercase row.
			if seen[canonical] {
				return result, nil, errors.New("supersession would create a self-link or cycle")
			}
			seen[canonical] = true
			if first {
				by, result.By, first = canonical, canonical, false
			}
			if !link.Valid {
				break
			}
			next = link.String
		}
		set := "superseded_by = ?"
		if kind == "decision" {
			set += ", status = 'superseded'"
			var status string
			if err := conn.QueryRowContext(ctx, "SELECT status FROM decisions WHERE id = ?", old).Scan(&status); err != nil {
				return result, nil, err
			}
			if status != "superseded" {
				prior.Valid = false
			}
		}
		if prior.Valid && prior.String == by {
			result.Status = "unchanged"
			return result, nil, nil
		}
		return result, []store.Statement{{SQL: "UPDATE " + table + " SET " + set + " WHERE id = ?", Args: []any{by, old}}}, nil
	})
}

// humanMemoryWrite binds only these six human methods. It shares proposalMu
// with existing owning-store mutations, not foreign Dolt sessions. Lookup and
// commit stay in one owner operation; no caller can supply SQL or an author.
func (s *Store) humanMemoryWrite(ctx context.Context, actor memory.Actor, text []string, operation string,
	prepare func(*sql.Conn) (HumanMemoryResult, []store.Statement, error),
) (result HumanMemoryResult, err error) {
	normalized, actorErr := memory.NormalizeActor(actor.Raw)
	if actorErr != nil || actor.Name != memory.UserActor.Name || normalized.Name != actor.Name {
		return result, errors.New("trusted human memory commands require the user actor; agents must propose changes for review")
	}
	text = append(text, actor.Name, actor.Raw)
	if err := humanText("raw actor", actor.Raw, 255); err != nil {
		return result, err
	}
	s.proposalMu.Lock()
	defer s.proposalMu.Unlock()
	conn, head, err := s.initializedMainConn(ctx, "human memory")
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, conn.Close()) }()
	if err := validateTransferSchema(ctx, conn, head); err != nil {
		return result, err
	}
	if err := requireCleanWorkingSet(ctx, conn, operation); err != nil {
		return result, err
	}
	var merging bool
	if err := conn.QueryRowContext(ctx, "SELECT is_merging FROM dolt_merge_status").Scan(&merging); err != nil {
		return result, err
	}
	if merging {
		return result, errors.New("human memory writes require main without an active merge; finish local work first")
	}
	if err := s.checkDenyList(text); err != nil {
		return result, err
	}
	var statements []store.Statement
	result, statements, err = prepare(conn)
	if err != nil {
		return HumanMemoryResult{}, err
	}
	if len(statements) == 0 {
		return result, nil
	}
	text = append(text, result.ID, result.By)
	current, err := branchHead(ctx, conn, MainBranch)
	if err != nil || current != head {
		return HumanMemoryResult{}, errors.Join(errors.New("main changed while preparing the human memory write; inspect before retrying"), err)
	}
	commit, err := s.commitConn(ctx, conn, store.CommitRequest{
		Statements: statements, Author: memory.UserActor.CommitAuthor(), Text: text,
		Message: operation + " " + result.ID, RequireClean: true,
	})
	if err != nil && commit.Hash == "" {
		return HumanMemoryResult{}, fmt.Errorf("%s outcome unconfirmed; inspect %s list before retrying: %w", operation, result.Kind, err)
	}
	result.Commit = commit.Hash
	return result, err
}

func resolveHumanFact(ctx context.Context, conn *sql.Conn, ident string) (id string, err error) {
	rows, err := conn.QueryContext(ctx, "SELECT id, `key` FROM facts WHERE id = ? OR `key` = ? ORDER BY id", ident, ident)
	if err != nil {
		return "", fmt.Errorf("resolve fact id or key: %w", err)
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		if id != "" {
			return "", errors.New("ambiguous fact key; select an exact fact id from fact list, including superseded rows")
		}
		var key sql.NullString
		if err := rows.Scan(&id, &key); err != nil {
			return "", err
		}
		if ident != id && (!key.Valid || key.String != ident) {
			return "", errors.New("fact identifier matches a different spelling under the database collation; use its exact id or key")
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if id == "" {
		return "", errors.New("no fact with that id or key")
	}
	return id, nil
}

func normalizedOptional(value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	return value
}

func optionalPointer(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func humanText(label, value string, chars int) error {
	if !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
		return fmt.Errorf("%s must be valid UTF-8 without NUL", label)
	}
	if chars > 0 && utf8.RuneCountInString(value) > chars {
		return fmt.Errorf("%s exceeds %d characters", label, chars)
	}
	if chars == 0 && len(value) > 65535 {
		return fmt.Errorf("%s exceeds the 65535-byte TEXT column", label)
	}
	return nil
}

func humanSource(source string) error {
	if err := humanText("source", source, 64); err != nil {
		return err
	}
	trimmed := strings.TrimSpace(source)
	if trimmed == "user" || trimmed == "git" || trimmed == "observed" {
		return nil
	}
	for _, prefix := range []string{"agent:", "user+agent:"} {
		id, found := strings.CutPrefix(trimmed, prefix)
		if found && id != "" && strings.Trim(id, "abcdefghijklmnopqrstuvwxyz0123456789-_.") == "" {
			return nil
		}
	}
	return errors.New("source must be user, git, observed, agent:<id>, or user+agent:<id> with a lowercase agent identifier")
}
