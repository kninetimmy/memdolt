package codeindex

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kninetimmy/memdolt/internal/layout"
)

const MaxFileHistoryLimit = 200000
const gitObservedFormat = "2006-01-02T15:04:05.000000000Z"

type FileHistoryHit struct {
	Path       string `json:"path"`
	CommitSHA  string `json:"commitSha"`
	Author     string `json:"author"`
	AuthoredAt string `json:"authoredAt"`
	Subject    string `json:"subject"`
	ChangeType string `json:"changeType"`
}

type HistoryCoverage struct {
	Cached        bool      `json:"cached"`
	Ranges        int       `json:"ranges"`
	LastIngest    *GitRange `json:"lastIngest"`
	Limit         int       `json:"limit"`
	Truncated     bool      `json:"truncated"`
	DeniedResults int       `json:"deniedResults"`
}

type FileHistory struct {
	Indexed  bool
	Results  []FileHistoryHit
	Coverage HistoryCoverage
}

// NormalizeHistoryPath accepts Windows query separators, but never trims path
// whitespace. Stored Git paths are validated separately and never rewritten.
func NormalizeHistoryPath(path string) (string, error) {
	path = strings.ReplaceAll(path, "\\", "/")
	if err := validateSourcePath(path); err != nil {
		return "", errors.New("file history requires a safe repository-relative path")
	}
	return path, nil
}

func (r *repository) historyPathAllowed(path string) (bool, error) {
	if validateSourcePath(path) != nil || defaultDenied(path) || r.cfg.denied.Check(path) != nil {
		return false, nil
	}
	// Only type/identity is inspected; history remains available after deletion.
	// Existing paths still cannot select linked sources or owner-file aliases.
	f, _, err := r.openSource(path)
	var protection *protectionError
	if errors.As(err, &protection) {
		return false, errors.New("cannot verify Git-history source path protection")
	}
	if os.IsNotExist(err) {
		return true, nil
	}
	if errors.Is(err, layout.ErrOwnerSource) || errors.Is(err, errUnsafePath) {
		return false, nil
	}
	if err != nil {
		return false, errors.New("cannot verify Git-history source path protection")
	}
	return true, f.Close()
}

// ReadFileHistory reads only the guarded cache and current path identities. It
// never runs Git, reads source bodies, migrates the index, or opens a model/Dolt.
// Missing/v1 history is empty; Indexed also recognizes locator-only paths.
func ReadFileHistory(ctx context.Context, start, path string, limit int) (result FileHistory, err error) {
	result.Results = []FileHistoryHit{}
	result.Coverage = HistoryCoverage{Cached: true, Limit: min(limit, MaxFileHistoryLimit)}
	if start == "" {
		return result, errors.New("file-history search requires the configured code repository root")
	}
	if validateSourcePath(path) != nil || limit < 1 {
		return result, errors.New("file history requires a safe repository-relative path and positive limit")
	}
	r, err := openRepository(start, false)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, r.close()) }()
	if ok, err := r.historyPathAllowed(path); err != nil || !ok {
		return result, errors.Join(errors.New("file-history path is denied or protected"), err)
	}
	if r.metadata == nil {
		return result, nil
	}
	if err := r.acquire(); err != nil {
		return result, err
	}
	db, _, err := r.openDB(ctx, false)
	if err != nil || db == nil {
		return result, err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	if err := checkOwnedSchema(ctx, db); err != nil {
		return result, err
	}
	v, err := storedVersion(ctx, db)
	if err != nil {
		return result, err
	}
	if err := needsRebuild(v); err != nil {
		return result, err
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return result, err
	}
	defer rollback(tx, &err)
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM indexed_files WHERE path=?)", path).Scan(&result.Indexed); err != nil {
		return result, err
	}
	if *v == 1 {
		return result, tx.Commit()
	}
	var known bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM git_files WHERE path=?)", path).Scan(&known); err != nil {
		return result, err
	}
	result.Indexed = result.Indexed || known
	result.Coverage, err = r.historyCoverage(ctx, tx, result.Coverage)
	if err != nil {
		return result, err
	}
	result.Results, result.Coverage.Truncated, result.Coverage.DeniedResults, err = r.fileHistoryRows(ctx, tx, path, result.Coverage.Limit)
	if err != nil {
		return result, err
	}
	return result, tx.Commit()
}

func (r *repository) historyCoverage(ctx context.Context, tx *sql.Tx, coverage HistoryCoverage) (HistoryCoverage, error) {
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM git_ingestions").Scan(&coverage.Ranges); err != nil {
		return coverage, err
	}
	if coverage.Ranges == 0 {
		return coverage, nil
	}
	var last GitRange
	var since, observed string
	if err := tx.QueryRowContext(ctx, "SELECT head,since_commit,shallow,observed_at FROM git_ingestions ORDER BY observed_at DESC,head,since_commit LIMIT 1").Scan(&last.Head, &since, &last.Shallow, &observed); err != nil {
		return coverage, err
	}
	when, err := time.Parse(time.RFC3339Nano, observed)
	if err != nil || when.UTC().Format(gitObservedFormat) != observed || !gitOID(last.Head) || since != "" && !gitOID(since) {
		return coverage, errors.New("invalid cached Git range metadata")
	}
	if r.cfg.denied.Check(last.Head, since, observed) != nil {
		return coverage, errors.New("cached Git range metadata matches a deny-list rule")
	}
	last.ObservedAt = when
	if since != "" {
		last.Since = &since
	}
	coverage.LastIngest = &last
	return coverage, nil
}

func (r *repository) fileHistoryRows(ctx context.Context, tx *sql.Tx, path string, limit int) (_ []FileHistoryHit, truncated bool, denied int, err error) {
	rows, err := tx.QueryContext(ctx, `SELECT f.path,c.sha,c.author,c.authored_at,c.authored_unix,c.subject,f.change_type
FROM git_commit_files f JOIN git_commits c ON c.sha=f.commit_sha
WHERE f.path=? ORDER BY c.authored_unix DESC,c.sha ASC LIMIT ?`, path, MaxFileHistoryLimit+1)
	if err != nil {
		return nil, false, 0, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	hits := []FileHistoryHit{}
	scanned := 0
	for rows.Next() {
		if scanned == MaxFileHistoryLimit {
			return hits, true, denied, nil
		}
		scanned++
		var hit FileHistoryHit
		var instant int64
		if err := rows.Scan(&hit.Path, &hit.CommitSHA, &hit.Author, &hit.AuthoredAt, &instant, &hit.Subject, &hit.ChangeType); err != nil {
			return nil, false, denied, err
		}
		when, parseErr := time.Parse(time.RFC3339, hit.AuthoredAt)
		if parseErr != nil || when.Unix() != instant || !gitOID(hit.CommitSHA) || hit.Path != path ||
			hit.Author == "" || !utf8.ValidString(hit.Author) || !utf8.ValidString(hit.Subject) || strings.ContainsAny(hit.Author+hit.Subject, "\x00") ||
			len(hit.ChangeType) != 1 || !strings.Contains("ADMTRC", hit.ChangeType) {
			return nil, false, denied, errors.New("invalid cached Git-history metadata")
		}
		if r.cfg.denied.Check(hit.Path, hit.CommitSHA, hit.Author, hit.AuthoredAt, hit.Subject, hit.ChangeType) != nil {
			denied++
			continue
		}
		if len(hits) == limit {
			return hits, true, denied, nil
		}
		hits = append(hits, hit)
	}
	return hits, false, denied, rows.Err()
}
