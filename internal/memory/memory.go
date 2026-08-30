// Package memory implements PRD §3.1's direct lanes: tasks, session notes,
// recorded commands and the project_state/project_arch narratives.
//
// These are the lanes with no review gate — memhub's semantics, kept — so
// every write here commits straight to main. What makes that safe to audit
// is commit metadata rather than an application-side log: each commit is
// authored by the normalized actor and carries a structured one-liner
// naming the operation, so dolt_log alone answers who wrote what and when
// (§3.1, §3.2). Nothing in this package writes an audit row, because the
// commit graph is the audit trail.
//
// One call here is one commit. §3.1's note batching — notes accumulating
// in the working set behind a five-minute timer — is MCP-server behavior
// for a long-lived session; a CLI process that exits after a single write
// has nothing to batch with.
//
// Every read and write goes through store.Store, the one path the CLI and
// the MCP server share (§5.1). This package opens no database of its own
// and knows nothing about where the store lives.
package memory

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/kninetimmy/memdolt/internal/store"
)

// ErrNotFound reports that a row the caller named does not exist.
var ErrNotFound = errors.New("not found")

// Lanes reads and writes one store's direct lanes, attributing every write
// to a single actor.
type Lanes struct {
	store store.Store
	actor Actor
}

// commandRecorder is the optional store-level operation that keeps a command
// recording's commit and read-back inside one store owner. An IPC-backed store
// implements it; LocalStore deliberately does not, so its direct path below
// remains the one implementation of the lane's SQL and commit metadata.
type commandRecorder interface {
	RecordCommand(context.Context, Actor, string, string, int) (Command, string, error)
}

// commandMu keeps every command recording's atomic upsert and read-back
// together, including recordings through distinct Lanes over the same store,
// so the returned row is the one persisted when that operation completes.
// SQL still performs the increment atomically for defense in depth.
// ponytail: process-global because command writes are rare; key locks by store
// if unrelated stores ever need concurrent command throughput in one process.
var commandMu sync.Mutex

// New returns the direct lanes of st. Writes are attributed to actor, both
// in the Dolt commit's authorship and in the actor columns of the rows
// that carry them (§6.1).
func New(st store.Store, actor Actor) *Lanes {
	return &Lanes{store: st, actor: actor}
}

// Actor reports the identity this Lanes attributes writes to.
func (l *Lanes) Actor() Actor { return l.actor }

// newID mints a row's primary key here, in the client, never in the
// database. PRD §6.1 keys every agent-writable table with a CHAR(26) ULID
// exactly so that two machines inserting between syncs cannot collide on a
// merge, which an AUTO_INCREMENT would. Minting client-side is also what
// makes an at-least-once write inspectable: the id exists before the
// statement runs, so a caller that has to retry knows which row it was
// writing (M0 rig 1, docs/spikes/m0-rig1.md). The random component comes
// directly from the operating system's cryptographic source.
var idEntropy = &ulid.LockedMonotonicReader{MonotonicReader: ulid.Monotonic(rand.Reader, 0)}

func newID() string { return newIDAt(time.Now()) }

func newIDAt(t time.Time) string {
	return ulid.MustNew(ulid.Timestamp(t), idEntropy).String()
}

// now is the timestamp a write stamps its rows with. It is truncated to
// the second because §6.1's DATETIME columns carry no fractional part:
// truncating here means the row a write returns is the row a later read
// gets back, instead of one that differs in digits the store dropped.
func now() time.Time { return time.Now().UTC().Truncate(time.Second) }

// write applies one statement and records it as one Dolt commit authored
// by the lane's actor (§3.1).
//
// text is every caller-originated string the write persists — a task title
// and its notes, a note's body and raw actor, a command line, a narrative
// body and raw actor — for the deny-list to match against (§11.3). Every
// direct lane has some, so none of them declares store.CommitRequest.NoText;
// a lane added here that passed none would be refused by the store on its
// first write rather than committed unscanned.
func (l *Lanes) write(ctx context.Context, message string, text []string, stmt store.Statement) (string, error) {
	result, err := l.store.Commit(ctx, store.CommitRequest{
		Statements: []store.Statement{stmt},
		Text:       text,
		Message:    message,
		Author:     l.actor.CommitAuthor(),
	})
	if err != nil {
		return "", err
	}
	return result.Hash, nil
}

// maxSummary bounds the free text a commit message quotes.
const maxSummary = 60

// summarize renders arbitrary text as the subject of a commit message: one
// line, bounded length. Both parts are load-bearing rather than cosmetic —
// store.CommitRequest rejects a multi-line message outright, because §3.1
// wants a one-liner, and a task title or a session note is whatever the
// caller typed. Collapsing the whitespace here is what stops a legitimate
// multi-line note from failing its own write.
func summarize(text string) string {
	joined := strings.Join(strings.Fields(text), " ")
	runes := []rune(joined)
	if len(runes) > maxSummary {
		return strings.TrimRight(string(runes[:maxSummary]), " ") + "..."
	}
	return joined
}

// nullable renders optional text for a NULL-able column: an empty string
// is stored as NULL rather than as an empty value, so "no notes" has one
// representation instead of two.
func nullable(text string) any {
	if text == "" {
		return nil
	}
	return text
}

// nullText reads a NULL-able string column, mapping NULL to "".
func nullText(value sql.NullString) string { return value.String }

// Task statuses, matching the tasks.status enum of §6.1.
const (
	StatusOpen    = "open"
	StatusDone    = "done"
	StatusBlocked = "blocked"

	// StatusAny is not a stored status. It is what Tasks takes to mean
	// "every status", so that a listing without a filter is spelled the
	// same way as one with it.
	StatusAny = "all"
)

// TaskStatuses are the statuses a task row may carry.
var TaskStatuses = []string{StatusOpen, StatusDone, StatusBlocked}

// Task is one row of the tasks table (§6.1).
type Task struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Status    string    `json:"status"`
	Notes     string    `json:"notes,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// AddTask opens a task and returns it with the hash of the commit that
// created it.
func (l *Lanes) AddTask(ctx context.Context, title, notes string) (Task, string, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return Task{}, "", errors.New("a task needs a title")
	}

	stamp := now()
	task := Task{
		ID:        newID(),
		Title:     title,
		Status:    StatusOpen,
		Notes:     strings.TrimSpace(notes),
		CreatedAt: stamp,
		UpdatedAt: stamp,
	}
	hash, err := l.write(ctx, "task add "+summarize(task.Title), []string{task.Title, task.Notes}, store.Statement{
		SQL: "INSERT INTO tasks (id, title, status, notes, created_at, updated_at) " +
			"VALUES (?, ?, ?, ?, ?, ?)",
		Args: []any{task.ID, task.Title, task.Status, nullable(task.Notes), task.CreatedAt, task.UpdatedAt},
	})
	if err != nil {
		return Task{}, "", fmt.Errorf("add task: %w", err)
	}
	return task, hash, nil
}

// CompleteTask marks a task done.
func (l *Lanes) CompleteTask(ctx context.Context, id string) (Task, string, error) {
	return l.setTaskStatus(ctx, id, StatusDone, "")
}

// BlockTask marks a task blocked. A non-empty notes replaces the task's
// notes — that is where the reason it is blocked goes; an empty one leaves
// them as they are.
func (l *Lanes) BlockTask(ctx context.Context, id, notes string) (Task, string, error) {
	return l.setTaskStatus(ctx, id, StatusBlocked, notes)
}

// setTaskStatus moves a task to status and commits the change.
//
// # The updated_at decision (PRD §6.3)
//
// Every task mutation here stamps updated_at, deliberately.
//
// §6.3 leaves this to the client and says why it matters: Dolt merges a
// row's cells independently, so two machines that edit disjoint columns of
// one task — status on one, title on the other — merge cleanly with no
// conflict, no constraint violation and no review opportunity. Writing
// only the columns a caller changed takes that path. Stamping updated_at
// on every mutation instead puts one cell in every task write that any
// concurrent task write also touches, so a same-row race always lands in
// dolt_conflicts_tasks and reaches an operator.
//
// Stamping is chosen because memdolt's writers are untrusted (§7) and its
// posture is fail-closed. A silent auto-merge of "one machine completed
// this task while another rewrote it" is precisely the outcome §6.3's
// done-versus-edit row exists to prevent, and that row's resolution policy
// — never pick a whole row by newest updated_at, preserve done unless an
// operator explicitly reopens it — presumes the conflict was raised at
// all. It also keeps the column honest: a mutation that left updated_at
// alone would leave the row claiming it had not changed.
//
// The cost is real and accepted: two machines that touch the same task
// between syncs always need an operator, even when their edits do not
// actually disagree. Tasks are few and short-lived, so that is rare, and a
// review that was not needed is the cheaper error.
func (l *Lanes) setTaskStatus(ctx context.Context, id, status, notes string) (Task, string, error) {
	task, err := l.Task(ctx, id)
	if err != nil {
		return Task{}, "", err
	}
	if task.Status == status {
		return Task{}, "", fmt.Errorf("task %s is already %s", task.ID, status)
	}

	task.Status = status
	task.UpdatedAt = now()
	stmt := store.Statement{
		SQL:  "UPDATE tasks SET status = ?, updated_at = ? WHERE id = ?",
		Args: []any{task.Status, task.UpdatedAt, task.ID},
	}
	if notes = strings.TrimSpace(notes); notes != "" {
		task.Notes = notes
		stmt = store.Statement{
			SQL:  "UPDATE tasks SET status = ?, updated_at = ?, notes = ? WHERE id = ?",
			Args: []any{task.Status, task.UpdatedAt, task.Notes, task.ID},
		}
	}

	// The notes are the only words this write records; a completion that
	// leaves them alone declares them anyway, empty, because "there is
	// nothing to scan here" is a claim the store makes a lane state.
	hash, err := l.write(ctx, "task "+taskOperation(status)+" "+task.ID, []string{notes}, stmt)
	if err != nil {
		return Task{}, "", fmt.Errorf("mark task %s %s: %w", task.ID, status, err)
	}
	return task, hash, nil
}

// taskOperation names the operation a status change is, for the commit
// message: the tool names of §11.1 are task_done and task_add, so the
// commit says "task done", not "task status done".
func taskOperation(status string) string {
	if status == StatusBlocked {
		return "block"
	}
	return status
}

// taskColumns is the column list every task read selects, in the order
// scanTasks expects.
const taskColumns = "id, title, status, notes, created_at, updated_at"

// Task returns one task by id.
func (l *Lanes) Task(ctx context.Context, id string) (Task, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return Task{}, errors.New("a task id is required")
	}
	tasks, err := l.queryTasks(ctx, "SELECT "+taskColumns+" FROM tasks WHERE id = ?", id)
	if err != nil {
		return Task{}, err
	}
	if len(tasks) == 0 {
		return Task{}, fmt.Errorf("task %s: %w", id, ErrNotFound)
	}
	return tasks[0], nil
}

// Tasks lists tasks of one status, oldest first. StatusAny (or an empty
// status) lists every task; anything else must be a status a task row can
// carry, so that a typo returns an error rather than an empty list that
// looks like an answer.
func (l *Lanes) Tasks(ctx context.Context, status string) ([]Task, error) {
	status = strings.TrimSpace(status)
	if status == "" || status == StatusAny {
		return l.queryTasks(ctx, "SELECT "+taskColumns+" FROM tasks ORDER BY created_at, id")
	}
	if err := validate("task status", status, TaskStatuses, StatusAny); err != nil {
		return nil, err
	}
	return l.queryTasks(ctx,
		"SELECT "+taskColumns+" FROM tasks WHERE status = ? ORDER BY created_at, id", status)
}

func (l *Lanes) queryTasks(ctx context.Context, query string, args ...any) (tasks []Task, err error) {
	rows, err := l.store.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read tasks: %w", err)
	}
	defer func() { err = errors.Join(err, rows.Close()) }()

	for rows.Next() {
		var task Task
		var notes sql.NullString
		if err := rows.Scan(&task.ID, &task.Title, &task.Status, &notes,
			&task.CreatedAt, &task.UpdatedAt); err != nil {
			return nil, fmt.Errorf("read tasks: %w", err)
		}
		task.Notes = nullText(notes)
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read tasks: %w", err)
	}
	return tasks, nil
}

// Note is one row of the session_notes table (§6.1).
type Note struct {
	ID       string `json:"id"`
	Actor    string `json:"actor"`
	ActorRaw string `json:"actorRaw"`
	Text     string `json:"text"`
	NoteProvenance
	CreatedAt time.Time `json:"createdAt"`
}

// NoteProvenance is optional host metadata attached to a session note. It is
// neither note text nor input to actor normalization (PRD §6.1, §11.4).
type NoteProvenance struct {
	SessionID  string `json:"session_id,omitempty"`
	AgentID    string `json:"agent_id,omitempty"`
	ProviderID string `json:"provider_id,omitempty"`
	ModelID    string `json:"model_id,omitempty"`
	Variant    string `json:"variant,omitempty"`
}

// LogNote is the short-lived CLI seam: it records one session note in one
// commit and returns both. The future M3 server must not call LogNote once per
// row: it accumulates note rows and commits that narrowed set on session end or
// the five-minute timer. It must not reuse a general DOLT_COMMIT('-A') path or
// weaken the clean-working-set guards around proposal staging and review.
func (l *Lanes) LogNote(ctx context.Context, body string) (Note, string, error) {
	return l.LogNoteWithProvenance(ctx, body, NoteProvenance{})
}

// LogNoteWithProvenance records one note with optional, independently
// verified host metadata. LogNote remains the ordinary no-metadata path.
func (l *Lanes) LogNoteWithProvenance(
	ctx context.Context,
	body string,
	provenance NoteProvenance,
) (Note, string, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return Note{}, "", errors.New("a session note needs text")
	}

	note := Note{
		ID:             newID(),
		Actor:          l.actor.Name,
		ActorRaw:       l.actor.Raw,
		Text:           body,
		NoteProvenance: provenance,
		CreatedAt:      now(),
	}
	text := []string{note.Text, note.ActorRaw}
	for _, value := range []string{note.SessionID, note.AgentID, note.ProviderID, note.ModelID, note.Variant} {
		if value != "" {
			text = append(text, value)
		}
	}
	hash, err := l.write(ctx, "note add "+summarize(note.Text), text, store.Statement{
		SQL: "INSERT INTO session_notes " +
			"(id, actor, actor_raw, text, session_id, agent_id, provider_id, model_id, variant, created_at) " +
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		Args: []any{
			note.ID, note.Actor, note.ActorRaw, note.Text,
			nullable(note.SessionID), nullable(note.AgentID), nullable(note.ProviderID),
			nullable(note.ModelID), nullable(note.Variant), note.CreatedAt,
		},
	})
	if err != nil {
		return Note{}, "", fmt.Errorf("log session note: %w", err)
	}
	return note, hash, nil
}

// Notes lists the most recent session notes, newest first. A limit of zero
// or less is refused rather than read as "no limit": an unbounded read of
// a table that grows every session is not something a caller asks for by
// leaving a flag at its zero value.
func (l *Lanes) Notes(ctx context.Context, limit int) (notes []Note, err error) {
	if limit <= 0 {
		return nil, fmt.Errorf("note limit %d must be positive", limit)
	}

	rows, err := l.store.Query(ctx,
		"SELECT id, actor, actor_raw, text, session_id, agent_id, provider_id, model_id, variant, created_at "+
			"FROM session_notes "+
			"ORDER BY created_at DESC, id DESC LIMIT ?", limit)
	if err != nil {
		return nil, fmt.Errorf("read session notes: %w", err)
	}
	defer func() { err = errors.Join(err, rows.Close()) }()

	for rows.Next() {
		var note Note
		var actor, actorRaw, sessionID, agentID, providerID, modelID, variant sql.NullString
		if err := rows.Scan(
			&note.ID, &actor, &actorRaw, &note.Text, &sessionID, &agentID,
			&providerID, &modelID, &variant, &note.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("read session notes: %w", err)
		}
		note.Actor, note.ActorRaw = nullText(actor), nullText(actorRaw)
		note.SessionID, note.AgentID = nullText(sessionID), nullText(agentID)
		note.ProviderID, note.ModelID, note.Variant = nullText(providerID), nullText(modelID), nullText(variant)
		notes = append(notes, note)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read session notes: %w", err)
	}
	return notes, nil
}

// CommandKinds are the kinds a recorded command may have, matching the
// commands.kind enum of §6.1.
var CommandKinds = []string{"build", "test", "run", "lint", "other"}

// Command is one row of the commands table (§6.1): the project's current
// command line of one kind, and how it has been faring.
//
// It is the one direct-lane table with no ULID key. Its primary key is the
// kind enum, because there is exactly one build command and one test
// command per project — recording a command replaces the row rather than
// appending to it.
type Command struct {
	Kind         string    `json:"kind"`
	Cmdline      string    `json:"cmdline"`
	LastExitCode int       `json:"lastExitCode"`
	LastRunAt    time.Time `json:"lastRunAt"`
	SuccessCount int       `json:"successCount"`
	FailCount    int       `json:"failCount"`
}

// RecordCommand records how a command of one kind ran: its command line,
// its exit code, and a tick on the success or failure tally.
//
// Before, this method read counters and upserted absolute values, so concurrent
// calls could read the same old value and lose an increment. After, the upsert
// increments in SQL, and commandMu covers the commit and read-back across
// Lanes, making each returned value agree with the row persisted at that
// operation's completion. Across machines the writes still touch the same
// cells and therefore retain the existing merge-conflict shape.
func (l *Lanes) RecordCommand(ctx context.Context, kind, cmdline string, exitCode int) (Command, string, error) {
	kind = strings.ToLower(strings.TrimSpace(kind))
	if err := validate("command kind", kind, CommandKinds); err != nil {
		return Command{}, "", err
	}
	if cmdline = strings.TrimSpace(cmdline); cmdline == "" {
		return Command{}, "", errors.New("a recorded command needs a command line")
	}
	if recorder, ok := l.store.(commandRecorder); ok {
		return recorder.RecordCommand(ctx, l.actor, kind, cmdline, exitCode)
	}

	commandMu.Lock()
	defer commandMu.Unlock()

	command := Command{Kind: kind, Cmdline: cmdline, LastExitCode: exitCode, LastRunAt: now()}
	if exitCode == 0 {
		command.SuccessCount = 1
	} else {
		command.FailCount = 1
	}

	hash, err := l.write(ctx,
		fmt.Sprintf("command record %s (exit %d)", command.Kind, command.LastExitCode),
		[]string{command.Cmdline},
		store.Statement{
			SQL: "INSERT INTO commands (kind, cmdline, last_exit_code, last_run_at, success_count, fail_count) " +
				"VALUES (?, ?, ?, ?, ?, ?) " +
				"ON DUPLICATE KEY UPDATE cmdline = ?, last_exit_code = ?, last_run_at = ?, " +
				"success_count = success_count + ?, fail_count = fail_count + ?",
			Args: []any{
				command.Kind, command.Cmdline, command.LastExitCode, command.LastRunAt,
				command.SuccessCount, command.FailCount,
				command.Cmdline, command.LastExitCode, command.LastRunAt,
				command.SuccessCount, command.FailCount,
			},
		})
	if err != nil {
		return Command{}, "", fmt.Errorf("record %s command: %w", kind, err)
	}
	persisted, err := l.Command(ctx, kind)
	if err != nil {
		return Command{}, "", fmt.Errorf("read back %s command after commit %s: %w", kind, hash, err)
	}
	return persisted, hash, nil
}

// Command returns the recorded command of one kind.
func (l *Lanes) Command(ctx context.Context, kind string) (command Command, err error) {
	kind = strings.ToLower(strings.TrimSpace(kind))
	if err := validate("command kind", kind, CommandKinds); err != nil {
		return Command{}, err
	}

	rows, err := l.store.Query(ctx,
		"SELECT kind, cmdline, last_exit_code, last_run_at, success_count, fail_count "+
			"FROM commands WHERE kind = ?", kind)
	if err != nil {
		return Command{}, fmt.Errorf("read %s command: %w", kind, err)
	}
	defer func() { err = errors.Join(err, rows.Close()) }()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return Command{}, fmt.Errorf("read %s command: %w", kind, err)
		}
		return Command{}, fmt.Errorf("no %s command recorded: %w", kind, ErrNotFound)
	}
	var cmdline sql.NullString
	if err := rows.Scan(&command.Kind, &cmdline, &command.LastExitCode, &command.LastRunAt,
		&command.SuccessCount, &command.FailCount); err != nil {
		return Command{}, fmt.Errorf("read %s command: %w", kind, err)
	}
	command.Cmdline = nullText(cmdline)
	return command, nil
}

// NarrativeKind names one of the two narrative tables.
type NarrativeKind string

// The narratives of §6.1: where the project's own description of its
// status and its architecture lives.
const (
	StateNarrative NarrativeKind = "state"
	ArchNarrative  NarrativeKind = "arch"
)

// NarrativeKinds are the narratives a caller may name.
var NarrativeKinds = []string{string(StateNarrative), string(ArchNarrative)}

// table maps a narrative onto its table. The result is one of two
// constants — never caller text — because a table name cannot be a bound
// parameter and so is the one part of these statements that is
// concatenated into SQL.
func (k NarrativeKind) table() (string, error) {
	switch k {
	case StateNarrative:
		return "project_state", nil
	case ArchNarrative:
		return "project_arch", nil
	}
	return "", fmt.Errorf("unknown narrative %q, want one of %s",
		string(k), strings.Join(NarrativeKinds, ", "))
}

// Narrative is one row of project_state or project_arch (§6.1): a written
// description of where the project stands or how it is built, as of when
// it was written.
type Narrative struct {
	Kind      NarrativeKind `json:"kind"`
	ID        string        `json:"id"`
	Body      string        `json:"body"`
	Actor     string        `json:"actor"`
	ActorRaw  string        `json:"actorRaw"`
	CreatedAt time.Time     `json:"createdAt"`
}

// SetNarrative records a new narrative body and returns it with the hash
// of the commit that carried it.
//
// It inserts a row rather than updating one. §3.2 makes the narrative's
// history a product feature — dolt_log over these two tables is the
// timeline of how the project described itself, and "how did our
// architecture evolve" is a query against it — and §6.1 shapes both tables
// for that: a ULID key and a created_at, with no updated_at and no unique
// key to update against. The current narrative is the newest row.
func (l *Lanes) SetNarrative(ctx context.Context, kind NarrativeKind, body string) (Narrative, string, error) {
	table, err := kind.table()
	if err != nil {
		return Narrative{}, "", err
	}
	if body = strings.TrimSpace(body); body == "" {
		return Narrative{}, "", fmt.Errorf("the %s narrative needs a body", kind)
	}

	narrative := Narrative{
		Kind:      kind,
		ID:        newID(),
		Body:      body,
		Actor:     l.actor.Name,
		ActorRaw:  l.actor.Raw,
		CreatedAt: now(),
	}
	hash, err := l.write(ctx, string(kind)+" set", []string{narrative.Body, narrative.ActorRaw}, store.Statement{
		SQL: "INSERT INTO " + table + " (id, body, actor, actor_raw, created_at) " +
			"VALUES (?, ?, ?, ?, ?)",
		Args: []any{narrative.ID, narrative.Body, narrative.Actor, narrative.ActorRaw, narrative.CreatedAt},
	})
	if err != nil {
		return Narrative{}, "", fmt.Errorf("set the %s narrative: %w", kind, err)
	}
	return narrative, hash, nil
}

// Narrative returns the current body of one narrative: the newest row of
// its table. A narrative nobody has written yet is ErrNotFound rather than
// an empty body, so "not set" and "set to nothing" stay distinguishable.
func (l *Lanes) Narrative(ctx context.Context, kind NarrativeKind) (narrative Narrative, err error) {
	table, err := kind.table()
	if err != nil {
		return Narrative{}, err
	}

	rows, err := l.store.Query(ctx,
		"SELECT id, body, actor, actor_raw, created_at FROM "+table+
			" ORDER BY created_at DESC, id DESC LIMIT 1")
	if err != nil {
		return Narrative{}, fmt.Errorf("read the %s narrative: %w", kind, err)
	}
	defer func() { err = errors.Join(err, rows.Close()) }()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return Narrative{}, fmt.Errorf("read the %s narrative: %w", kind, err)
		}
		return Narrative{}, fmt.Errorf("no %s narrative recorded: %w", kind, ErrNotFound)
	}
	narrative.Kind = kind
	var body, actor, actorRaw sql.NullString
	if err := rows.Scan(&narrative.ID, &body, &actor, &actorRaw, &narrative.CreatedAt); err != nil {
		return Narrative{}, fmt.Errorf("read the %s narrative: %w", kind, err)
	}
	narrative.Body, narrative.Actor, narrative.ActorRaw = nullText(body), nullText(actor), nullText(actorRaw)
	return narrative, nil
}

// validate reports whether value is one of the allowed values, naming what
// was expected when it is not. Enum columns reject an unknown value with
// "Data truncated for column", which says nothing about what the caller
// should have written, so these are checked before they reach the store.
func validate(label, value string, allowed []string, extra ...string) error {
	for _, candidate := range allowed {
		if value == candidate {
			return nil
		}
	}
	return fmt.Errorf("unknown %s %q, want one of %s",
		label, value, strings.Join(append(append([]string{}, allowed...), extra...), ", "))
}
