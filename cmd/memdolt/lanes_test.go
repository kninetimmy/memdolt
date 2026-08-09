package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"github.com/kninetimmy/memdolt/internal/memory"
)

// TestTaskLaneRoundTrip covers the tasks lane end to end: a task can be
// opened, completed and blocked from the CLI, and the listing answers back
// what was written (acceptance criteria 1 and 4).
func TestTaskLaneRoundTrip(t *testing.T) {
	base := initStore(t)

	first := decodeJSON[taskInfo](t, runMemdolt(t,
		"task", "add", "wire the direct lanes", "--notes", "PRD §3.1", "--dir", base, "--json"))
	second := decodeJSON[taskInfo](t, runMemdolt(t,
		"task", "add", "record the test command", "--dir", base, "--json"))

	if first.Status != memory.StatusOpen {
		t.Fatalf("a new task has status %q, want %q", first.Status, memory.StatusOpen)
	}
	if first.Notes != "PRD §3.1" {
		t.Fatalf("the new task's notes are %q, want %q", first.Notes, "PRD §3.1")
	}

	// The default listing is the open ones, and it round-trips both.
	open := decodeJSON[taskList](t, runMemdolt(t, "task", "list", "--dir", base, "--json"))
	if got := taskIDs(open.Tasks); !equalStrings(got, []string{first.ID, second.ID}) {
		t.Fatalf("open tasks are %v, want %v", got, []string{first.ID, second.ID})
	}
	if human := runMemdolt(t, "task", "list", "--dir", base); !strings.Contains(human, "wire the direct lanes") {
		t.Fatalf("`task list` printed %q, want it to name the open task", human)
	}

	done := decodeJSON[taskInfo](t, runMemdolt(t, "task", "done", first.ID, "--dir", base, "--json"))
	if done.Status != memory.StatusDone {
		t.Fatalf("the completed task has status %q, want %q", done.Status, memory.StatusDone)
	}
	// Every task mutation stamps updated_at (the §6.3 decision recorded in
	// internal/memory), so completing a task moves it.
	if !done.UpdatedAt.After(first.UpdatedAt) && !done.UpdatedAt.Equal(first.UpdatedAt) {
		t.Fatalf("completing the task moved updated_at backwards: %s then %s", first.UpdatedAt, done.UpdatedAt)
	}

	blocked := decodeJSON[taskInfo](t, runMemdolt(t,
		"task", "block", second.ID, "--notes", "waiting on the schema", "--dir", base, "--json"))
	if blocked.Status != memory.StatusBlocked || blocked.Notes != "waiting on the schema" {
		t.Fatalf("the blocked task is %+v, want status %q with the reason recorded",
			blocked, memory.StatusBlocked)
	}

	// Each status filter returns what was written to it, and the default
	// filter now returns nothing.
	for _, tc := range []struct {
		status string
		want   []string
	}{
		{status: memory.StatusOpen, want: nil},
		{status: memory.StatusDone, want: []string{first.ID}},
		{status: memory.StatusBlocked, want: []string{second.ID}},
		{status: memory.StatusAny, want: []string{first.ID, second.ID}},
	} {
		listed := decodeJSON[taskList](t, runMemdolt(t,
			"task", "list", "--status", tc.status, "--dir", base, "--json"))
		if got := taskIDs(listed.Tasks); !equalStrings(got, tc.want) {
			t.Errorf("`task list --status %s` returned %v, want %v", tc.status, got, tc.want)
		}
	}
}

// TestTaskLaneRefusalsAreLoud covers the errors a mistyped task command
// has to give back rather than swallow.
func TestTaskLaneRefusalsAreLoud(t *testing.T) {
	base := initStore(t)
	task := decodeJSON[taskInfo](t, runMemdolt(t, "task", "add", "a task", "--dir", base, "--json"))
	runMemdolt(t, "task", "done", task.ID, "--dir", base)

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{
			name: "unknown id",
			args: []string{"task", "done", "01ARZ3NDEKTSV4RRFFQ69G5FAV", "--dir", base},
			want: "not found",
		},
		{
			name: "already done",
			args: []string{"task", "done", task.ID, "--dir", base},
			want: "already done",
		},
		{
			name: "unknown status filter",
			args: []string{"task", "list", "--status", "finished", "--dir", base},
			want: "unknown task status",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := runMemdoltErr(t, tc.args...); !strings.Contains(err, tc.want) {
				t.Fatalf("error was %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestSessionNoteLaneRoundTrip covers the notes lane: a note logged from
// the CLI is attributed to the normalized actor and reads back verbatim,
// including one that spans lines — a commit message may not (§3.1), so a
// multi-line note is the case that proves the message is summarized rather
// than the note being mangled to fit one.
func TestSessionNoteLaneRoundTrip(t *testing.T) {
	base := initStore(t)

	const multiline = "picked up the direct lanes\n\nthe updated_at question is settled in §6.3"
	note := decodeJSON[noteInfo](t, runMemdoltIn(t, multiline,
		"note", "add", "--actor", "Claude Code", "--dir", base, "--json"))

	if note.Text != multiline {
		t.Fatalf("the logged note is %q, want the text as written %q", note.Text, multiline)
	}
	if note.Actor != "agent:claude-code" || note.ActorRaw != "Claude Code" {
		t.Fatalf("the note is attributed to %q (raw %q), want %q (raw %q)",
			note.Actor, note.ActorRaw, "agent:claude-code", "Claude Code")
	}

	runMemdolt(t, "note", "add", "and the CLI commits one note per invocation", "--dir", base)

	listed := decodeJSON[noteList](t, runMemdolt(t, "note", "list", "--dir", base, "--json"))
	if len(listed.Notes) != 2 {
		t.Fatalf("the note listing has %d notes, want 2", len(listed.Notes))
	}
	// Newest first.
	if listed.Notes[0].Actor != "user" || listed.Notes[1].ID != note.ID {
		t.Fatalf("the note listing is %+v, want the newest note first", listed.Notes)
	}
	if human := runMemdolt(t, "note", "list", "--dir", base); !strings.Contains(human, "picked up the direct lanes") {
		t.Fatalf("`note list` printed %q, want it to carry the note text", human)
	}
}

// TestCommandLaneRoundTrip covers the commands lane: recording a run
// replaces the stored command line, keeps the tallies, and the lookup by
// kind answers with what was recorded (acceptance criteria 1 and 4).
func TestCommandLaneRoundTrip(t *testing.T) {
	base := initStore(t)

	recorded := decodeJSON[commandInfo](t, runMemdolt(t,
		"command", "record", "test", "go test ./...", "--dir", base, "--json"))
	if recorded.SuccessCount != 1 || recorded.FailCount != 0 || recorded.LastExitCode != 0 {
		t.Fatalf("the first recorded run is %+v, want one success and no failures", recorded)
	}

	failed := decodeJSON[commandInfo](t, runMemdolt(t,
		"command", "record", "test", "go test -race ./...", "--exit", "1", "--dir", base, "--json"))
	if failed.SuccessCount != 1 || failed.FailCount != 1 || failed.LastExitCode != 1 {
		t.Fatalf("the failed run is %+v, want the tallies at one each and exit 1", failed)
	}

	// Recording one kind leaves the others alone: the table is keyed by
	// kind, so build and test are two rows.
	runMemdolt(t, "command", "record", "build", "go build ./...", "--dir", base)

	got := decodeJSON[memory.Command](t, runMemdolt(t, "command", "get", "test", "--dir", base, "--json"))
	if got.Cmdline != "go test -race ./..." || got.SuccessCount != 1 || got.FailCount != 1 {
		t.Fatalf("`command get test` returned %+v, want the newest command line and both tallies", got)
	}
	if human := runMemdolt(t, "command", "get", "build", "--dir", base); !strings.Contains(human, "go build ./...") {
		t.Fatalf("`command get build` printed %q, want it to carry the command line", human)
	}

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{
			name: "kind that is not in the enum",
			args: []string{"command", "record", "deploy", "make deploy", "--dir", base},
			want: "unknown command kind",
		},
		{
			name: "kind nobody has recorded",
			args: []string{"command", "get", "lint", "--dir", base},
			want: "not found",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := runMemdoltErr(t, tc.args...); !strings.Contains(err, tc.want) {
				t.Fatalf("error was %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestNarrativeLanesRoundTrip covers project_state and project_arch:
// setting a narrative appends a version, reading it returns the newest,
// and the two tables are independent (acceptance criteria 1 and 4).
func TestNarrativeLanesRoundTrip(t *testing.T) {
	base := initStore(t)

	for _, kind := range []string{"state", "arch"} {
		if err := runMemdoltErr(t, kind, "show", "--dir", base); !strings.Contains(err, "not found") {
			t.Fatalf("`%s show` on an unwritten narrative said %q, want a not-found error", kind, err)
		}
	}

	first := decodeJSON[narrativeInfo](t, runMemdolt(t,
		"state", "set", "M1 is landing the direct lanes", "--dir", base, "--json"))
	const second = "M1 direct lanes are in review\nnext up: recall"
	runMemdoltIn(t, second, "state", "set", "--dir", base)
	runMemdolt(t, "arch", "set", "cli and mcp share one Store (§5.1)", "--dir", base)

	// Reading a narrative returns its newest version, and the body is what
	// a human-readable read prints, so that it can be redirected to a file.
	if got := strings.TrimRight(runMemdolt(t, "state", "show", "--dir", base), "\n"); got != second {
		t.Fatalf("`state show` printed %q, want the newest body %q", got, second)
	}
	current := decodeJSON[memory.Narrative](t, runMemdolt(t, "state", "show", "--dir", base, "--json"))
	if current.Body != second || current.ID == first.ID {
		t.Fatalf("`state show --json` returned %+v, want the second version's row", current)
	}
	if got := strings.TrimRight(runMemdolt(t, "arch", "show", "--dir", base), "\n"); !strings.Contains(got, "§5.1") {
		t.Fatalf("`arch show` printed %q, want the architecture narrative", got)
	}

	// Setting a narrative appends rather than overwrites: §3.2 makes the
	// history of the project's self-description a product feature.
	if got := countRows(t, base, "SELECT COUNT(*) FROM project_state"); got != 2 {
		t.Fatalf("project_state has %d rows after two writes, want 2", got)
	}
}

// TestDirectLaneCommitsCarryTheActorAndAStructuredMessage is acceptance
// criteria 1 and 3: one Dolt commit on main per write, authored by the
// normalized actor and messaged with a one-liner naming the operation, so
// that a dolt_log query alone answers who wrote what and when.
func TestDirectLaneCommitsCarryTheActorAndAStructuredMessage(t *testing.T) {
	base := initStore(t)
	before := storeCommitCount(t, base)

	const agent = "Claude Code"
	task := decodeJSON[taskInfo](t, runMemdolt(t,
		"task", "add", "wire the direct lanes", "--actor", agent, "--dir", base, "--json"))
	done := decodeJSON[taskInfo](t, runMemdolt(t,
		"task", "done", task.ID, "--actor", "codex", "--dir", base, "--json"))
	note := decodeJSON[noteInfo](t, runMemdoltIn(t, "lanes committed\none per invocation",
		"note", "add", "--actor", agent, "--dir", base, "--json"))
	command := decodeJSON[commandInfo](t, runMemdolt(t,
		"command", "record", "test", "go test ./...", "--exit", "1", "--dir", base, "--json"))
	state := decodeJSON[narrativeInfo](t, runMemdolt(t,
		"state", "set", "lanes land next", "--dir", base, "--json"))
	arch := decodeJSON[narrativeInfo](t, runMemdolt(t,
		"arch", "set", "cli over internal/memory over the store", "--actor", "agent:codex", "--dir", base, "--json"))

	// One commit per write, and no write that produced none.
	if got, want := storeCommitCount(t, base), before+6; got != want {
		t.Fatalf("six direct-lane writes left %d commits, want %d", got, want)
	}
	if branch := activeBranch(t, base); branch != "main" {
		t.Fatalf("the direct lanes committed on %q, want main (PRD §3.1)", branch)
	}

	const (
		claude      = "agent:claude-code"
		claudeEmail = "agent-claude-code@memdolt.invalid"
		codex       = "agent:codex"
		codexEmail  = "agent-codex@memdolt.invalid"
	)
	log := commitLog(t, base)
	for _, want := range []struct {
		what    string
		hash    string
		author  string
		email   string
		message string
	}{
		{what: "task add", hash: task.Commit, author: claude, email: claudeEmail,
			message: "task add wire the direct lanes"},
		{what: "task done", hash: done.Commit, author: codex, email: codexEmail,
			message: "task done " + task.ID},
		{what: "note add", hash: note.Commit, author: claude, email: claudeEmail,
			// The note spans two lines; a commit message may not.
			message: "note add lanes committed one per invocation"},
		{what: "command record", hash: command.Commit, author: "user", email: "user@memdolt.invalid",
			message: "command record test (exit 1)"},
		{what: "state set", hash: state.Commit, author: "user", email: "user@memdolt.invalid",
			message: "state set"},
		{what: "arch set", hash: arch.Commit, author: codex, email: codexEmail,
			message: "arch set"},
	} {
		entry, ok := log[want.hash]
		if !ok {
			t.Errorf("the %s commit %q is not in dolt_log", want.what, want.hash)
			continue
		}
		if entry.author != want.author || entry.email != want.email {
			t.Errorf("the %s commit is authored by %q <%s>, want %q <%s>",
				want.what, entry.author, entry.email, want.author, want.email)
		}
		if entry.message != want.message {
			t.Errorf("the %s commit message is %q, want %q", want.what, entry.message, want.message)
		}
	}
}

// TestLongSubjectsAreSummarizedIntoTheCommitMessage covers the other half
// of the one-liner contract: free text longer than a commit subject is cut
// down rather than committed whole.
func TestLongSubjectsAreSummarizedIntoTheCommitMessage(t *testing.T) {
	base := initStore(t)

	title := "wire the direct lanes so that dolt_log alone answers who wrote what and when, " +
		"with no application-side audit table"
	task := decodeJSON[taskInfo](t, runMemdolt(t, "task", "add", title, "--dir", base, "--json"))

	message := commitLog(t, base)[task.Commit].message
	if !strings.HasPrefix(message, "task add wire the direct lanes so that") {
		t.Fatalf("the commit message is %q, want it to open with the task title", message)
	}
	if !strings.HasSuffix(message, "...") || len([]rune(message)) > len("task add ")+63 {
		t.Fatalf("the commit message is %q (%d runes), want the title truncated",
			message, len([]rune(message)))
	}
}

// TestDirectLaneRowsCarryClientMintedULIDs is acceptance criterion 2:
// every row a direct lane inserts is keyed by a CHAR(26) ULID minted by
// the client, which is what makes two machines' inserts merge-safe (§6.1).
// commands is excluded by the schema, not by oversight: it is keyed by its
// kind enum.
func TestDirectLaneRowsCarryClientMintedULIDs(t *testing.T) {
	base := initStore(t)

	runMemdolt(t, "task", "add", "a task", "--dir", base)
	runMemdolt(t, "note", "add", "a note", "--dir", base)
	runMemdolt(t, "state", "set", "a status", "--dir", base)
	runMemdolt(t, "arch", "set", "an architecture", "--dir", base)

	for _, table := range []string{"tasks", "session_notes", "project_state", "project_arch"} {
		ids := queryStrings(t, base, "SELECT id FROM "+table)
		if len(ids) != 1 {
			t.Fatalf("%s has %d rows, want the one the lane wrote", table, len(ids))
		}
		for _, id := range ids {
			if len(id) != 26 {
				t.Errorf("%s row id %q is %d characters, want the 26 of a ULID", table, id, len(id))
			}
			if _, err := ulid.ParseStrict(id); err != nil {
				t.Errorf("%s row id %q does not parse as a ULID: %v", table, id, err)
			}
		}
	}
}

// TestLaneCommandsRefuseAnUninitializedStore covers the guard that names
// the command to run instead of failing later as a missing table.
func TestLaneCommandsRefuseAnUninitializedStore(t *testing.T) {
	base := scratchDir(t)

	err := runMemdoltErr(t, "task", "add", "a task", "--dir", base)
	if !strings.Contains(err, "memdolt init") {
		t.Fatalf("writing to an uninitialized store said %q, want it to name `memdolt init`", err)
	}
}

// initStore makes a scratch repository with an initialized store in it.
func initStore(t *testing.T) string {
	t.Helper()
	base := scratchDir(t)
	runMemdolt(t, "init", "--dir", base)
	return base
}

// runMemdoltIn runs a command with text on its standard input.
func runMemdoltIn(t *testing.T, stdin string, args ...string) string {
	t.Helper()
	root := newRootCommand()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		t.Fatalf("execute `memdolt %s`: %v (output %q)", strings.Join(args, " "), err, out)
	}
	return out.String()
}

// runMemdoltErr runs a command that is expected to fail and returns the
// error it reported.
func runMemdoltErr(t *testing.T, args ...string) string {
	t.Helper()
	root := newRootCommand()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs(args)
	err := root.Execute()
	if err == nil {
		t.Fatalf("`memdolt %s` succeeded with output %q, want an error", strings.Join(args, " "), out)
	}
	return err.Error()
}

func decodeJSON[T any](t *testing.T, out string) T {
	t.Helper()
	var value T
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &value); err != nil {
		t.Fatalf("decode %q as json: %v", out, err)
	}
	return value
}

type taskList struct {
	Tasks []memory.Task `json:"tasks"`
}

type noteList struct {
	Notes []memory.Note `json:"notes"`
}

func taskIDs(tasks []memory.Task) []string {
	ids := make([]string, 0, len(tasks))
	for _, task := range tasks {
		ids = append(ids, task.ID)
	}
	return ids
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// logEntry is one row of dolt_log: the provenance a commit carries.
type logEntry struct {
	author  string
	email   string
	message string
}

// commitLog reads the store's history keyed by commit hash, so that a test
// looks up the commit a write reported rather than depending on the order
// two commits a millisecond apart come back in.
func commitLog(t *testing.T, base string) map[string]logEntry {
	t.Helper()
	st := openInitializedStore(t, base)
	defer func() { _ = st.Close() }()

	rows, err := st.Query(context.Background(),
		"SELECT commit_hash, committer, email, message FROM dolt_log")
	if err != nil {
		t.Fatalf("read dolt_log: %v", err)
	}
	defer func() { _ = rows.Close() }()

	log := map[string]logEntry{}
	for rows.Next() {
		var hash string
		var entry logEntry
		if err := rows.Scan(&hash, &entry.author, &entry.email, &entry.message); err != nil {
			t.Fatalf("scan dolt_log: %v", err)
		}
		log[hash] = entry
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read dolt_log: %v", err)
	}
	return log
}

func activeBranch(t *testing.T, base string) string {
	t.Helper()
	branches := queryStrings(t, base, "SELECT active_branch()")
	if len(branches) != 1 {
		t.Fatalf("active_branch() returned %d rows, want 1", len(branches))
	}
	return branches[0]
}

func queryStrings(t *testing.T, base, query string, args ...any) []string {
	t.Helper()
	st := openInitializedStore(t, base)
	defer func() { _ = st.Close() }()

	rows, err := st.Query(context.Background(), query, args...)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	defer func() { _ = rows.Close() }()

	var values []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			t.Fatalf("scan %q: %v", query, err)
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return values
}

func countRows(t *testing.T, base, query string, args ...any) int {
	t.Helper()
	counts := queryStrings(t, base, query, args...)
	if len(counts) != 1 {
		t.Fatalf("%q returned %d rows, want 1", query, len(counts))
	}
	count, err := strconv.Atoi(counts[0])
	if err != nil {
		t.Fatalf("%q returned %q, want a count: %v", query, counts[0], err)
	}
	return count
}
