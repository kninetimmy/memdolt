package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/kninetimmy/memdolt/internal/ipc"
	"github.com/kninetimmy/memdolt/internal/memory"
	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
	"github.com/kninetimmy/memdolt/internal/storeipc"
)

// This file is the CLI over PRD §3.1's direct lanes: tasks, session notes,
// recorded commands and the project_state/project_arch narratives. Every
// command here is a thin shell over internal/memory — the shell parses
// flags and renders output, and the lane package owns the SQL and the
// commit metadata, because the MCP server of §11.1 offers these same
// operations and must not reach the store down a second path (§5.1).

// storeFlags are the flags a direct-lane command needs to find its store
// and attribute its writes.
type storeFlags struct {
	dir    string
	actor  string
	global bool
}

// commandStore is the complete initialized store surface shipped commands
// use. LocalStore and the live owner's IPC client both implement it.
type commandStore interface {
	storeipc.Backend
	DataDir() string
	ReviewAccept(context.Context, string, store.Actor, bool) (localdolt.AcceptResult, error)
}

// bind adds the flags a read needs.
func (f *storeFlags) bind(cmd *cobra.Command) *cobra.Command {
	cmd.Flags().StringVar(&f.dir, "dir", ".",
		"repository root whose store to use (the store lives in <dir>/.memdolt)")
	return cmd
}

// bindWriter adds the flags a write needs: a read's, plus the identity the
// commit is authored by.
func (f *storeFlags) bindWriter(cmd *cobra.Command) *cobra.Command {
	f.bind(cmd)
	cmd.Flags().StringVar(&f.actor, "actor", "",
		"who to attribute the write to: empty or \"user\" for a person, any other name for "+
			"an agent, recorded as agent:<name> (PRD §3.1)")
	return cmd
}

func (f *storeFlags) bindLaneWriter(cmd *cobra.Command) *cobra.Command {
	if cmd.Long == "" {
		cmd.Long = cmd.Short
	}
	cmd.Long += "\n\nA late failure retains any confirmed row identity and commit hash; inspect the\n" +
		"named row and Dolt history before retrying. A lost owner reply means outcome\n" +
		"unknown, never an automatic replay. Success output remains unchanged."
	return f.bindWriter(cmd)
}

// run opens the store the flags name, hands its direct lanes to fn and
// closes it again.
func (f *storeFlags) run(cmd *cobra.Command, fn func(context.Context, *memory.Lanes) error) error {
	return f.runStore(cmd, func(ctx context.Context, st commandStore, actor memory.Actor) error {
		return fn(ctx, memory.New(st, actor))
	})
}

// runLaneRead keeps note, command and narrative reads from creating an absent
// database through Open. Writer routing and other command families stay separate.
func (f *storeFlags) runLaneRead(cmd *cobra.Command, fn func(context.Context, *memory.Lanes) error) error {
	if err := localdolt.RequireExistingTransferStore(f.dir); err != nil {
		return err
	}
	return f.run(cmd, fn)
}

// runLaneWrite closes the selected store before reporting a direct write, so
// close failures cannot hide behind an already-emitted success.
func runLaneWrite[T any](cmd *cobra.Command, flags *storeFlags, write func(context.Context, *memory.Lanes) (T, string, error)) (T, string, error) {
	var result T
	var commit string
	err := flags.run(cmd, func(ctx context.Context, lanes *memory.Lanes) error {
		var err error
		result, commit, err = write(ctx, lanes)
		return err
	})
	return result, commit, err
}

func emitLaneWrite(cmd *cobra.Command, payload any, commit, inspect string, lines []string, err error) error {
	if err != nil && commit == "" {
		return err
	}
	if err != nil {
		lines = append(lines, "error: "+err.Error())
	}
	return memory.ConfirmedWriteError(errors.Join(err, emit(cmd, payload, lines)), commit, inspect)
}

func laneError(err error) string {
	if err != nil {
		return err.Error()
	}
	return ""
}

// runStore opens the store the flags name, hands it to fn and closes it
// again. The direct lanes reach it through run, which wraps the store in
// internal/memory; review (review.go) needs the store itself, because a
// proposal branch is not one of §3.1's lanes.
func (f *storeFlags) runStore(cmd *cobra.Command, fn func(context.Context, commandStore, memory.Actor) error) (err error) {
	actor, err := memory.NormalizeActor(f.actor)
	if err != nil {
		return err
	}

	ctx := cmd.Context()
	st, err := f.open(ctx, actor.CommitAuthor())
	if err != nil {
		return err
	}
	// The store holds the single-owner lock (PRD §5.2) until it is closed,
	// and closing is itself a write path, so its failure is reported rather
	// than swallowed.
	defer func() { err = errors.Join(err, st.Close()) }()

	if err := requireCurrentSchema(ctx, st); err != nil {
		return err
	}
	return fn(ctx, st, actor)
}

// openCommandStore routes through a verified live owner and opens embedded
// Dolt directly only when Probe proves there is no live owner. Probe failures
// fail closed: guessing "no owner" would violate PRD §5.2's single-owner rule.
func openCommandStore(ctx context.Context, baseDir string, actor store.Actor) (commandStore, error) {
	if _, err := localdolt.ReadRepoConfig(baseDir); err != nil {
		return nil, err
	}
	status, _, err := ipc.Probe(ctx, baseDir)
	if err != nil {
		return nil, fmt.Errorf("check for a live store owner: %w", err)
	}
	if status == ipc.StatusOwnerLive {
		st, err := storeipc.DialOwnerStore(baseDir)
		if err != nil {
			return nil, fmt.Errorf("connect to the live store owner: %w", err)
		}
		if err := st.Open(ctx); err != nil {
			return nil, err
		}
		return st, nil
	}

	st, err := localdolt.New(localdolt.Config{BaseDir: baseDir, Actor: actor})
	if err != nil {
		return nil, err
	}
	if err := st.Open(ctx); err != nil {
		return nil, err
	}
	return &localCommandStore{Store: st, baseDir: baseDir}, nil
}

// requireCurrentSchema refuses a store the shipped statements would not
// fit. Open already turns away a store newer than this binary (§6.4); this
// is the other direction, where the tables a lane writes may not exist
// yet, and it names the command that fixes it rather than letting the
// write fail as "table not found".
func requireCurrentSchema(ctx context.Context, st commandStore) error {
	version, err := st.SchemaVersion(ctx)
	if err != nil {
		return err
	}
	latest := store.LatestSchemaVersion()
	switch {
	case version == 0:
		return fmt.Errorf("there is no memdolt store at %s yet: run `memdolt init` first", st.DataDir())
	case version < latest:
		return fmt.Errorf("the memdolt store at %s is at schema v%d and this binary needs v%d: "+
			"run `memdolt init` to apply the missing migrations", st.DataDir(), version, latest)
	}
	return nil
}

// emit writes a command's result: one JSON object when --json is set, the
// given human-readable lines otherwise.
func emit(cmd *cobra.Command, payload any, lines []string) error {
	out := cmd.OutOrStdout()
	if jsonOutput {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("encode the %s result as json: %w", cmd.Name(), err)
		}
		if _, err := fmt.Fprintln(out, string(encoded)); err != nil {
			return fmt.Errorf("write the %s json: %w", cmd.Name(), err)
		}
		return nil
	}
	for _, line := range lines {
		if _, err := fmt.Fprintln(out, line); err != nil {
			return fmt.Errorf("write the %s output: %w", cmd.Name(), err)
		}
	}
	return nil
}

// bodyArg reads the body text of a write: the argument when there is one,
// standard input otherwise, so that prose can be piped in
// (`memdolt state set < STATUS.md`) instead of quoted into a shell
// argument.
func bodyArg(cmd *cobra.Command, args []string) (string, error) {
	if len(args) > 0 {
		return args[0], nil
	}
	body, err := io.ReadAll(cmd.InOrStdin())
	if err != nil {
		return "", fmt.Errorf("read the %s body from stdin: %w", cmd.Name(), err)
	}
	return string(body), nil
}

// stamp renders a stored timestamp for a human-readable line.
func stamp(at time.Time) string { return at.Format(time.RFC3339) }

// taskInfo is what a task write prints: the row, plus the commit that
// carries it.
type taskInfo struct {
	memory.Task
	Commit string `json:"commit"`
	Error  string `json:"error,omitempty"`
}

func newTaskCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "task",
		Short: "Open, complete, block and list tasks",
		Long: "Tasks are a direct lane (PRD §3.1): every change commits straight to main,\n" +
			"authored by the actor that made it.",
	}
	cmd.AddCommand(newTaskAddCommand(), newTaskDoneCommand(), newTaskBlockCommand(), newTaskListCommand())
	return cmd
}

func newTaskAddCommand() *cobra.Command {
	var flags storeFlags
	var notes string

	cmd := &cobra.Command{
		Use:   "add <title>",
		Short: "Open a task",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			task, commit, err := runLaneWrite(cmd, &flags, func(ctx context.Context, lanes *memory.Lanes) (memory.Task, string, error) {
				return lanes.AddTask(ctx, args[0], notes)
			})
			return emitLaneWrite(cmd, taskInfo{Task: task, Commit: commit, Error: laneError(err)}, commit,
				"`memdolt task list --status all` for task "+task.ID,
				[]string{fmt.Sprintf("opened task %s (commit %s)", task.ID, commit)}, err)
		},
	}
	cmd.Flags().StringVar(&notes, "notes", "", "detail to record alongside the title")

	return flags.bindLaneWriter(cmd)
}

func newTaskDoneCommand() *cobra.Command {
	var flags storeFlags

	cmd := &cobra.Command{
		Use:   "done <id>",
		Short: "Mark a task done",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			task, commit, err := runLaneWrite(cmd, &flags, func(ctx context.Context, lanes *memory.Lanes) (memory.Task, string, error) {
				return lanes.CompleteTask(ctx, args[0])
			})
			return emitLaneWrite(cmd, taskInfo{Task: task, Commit: commit, Error: laneError(err)}, commit,
				"`memdolt task list --status all` for task "+task.ID,
				[]string{fmt.Sprintf("task %s is done (commit %s)", task.ID, commit)}, err)
		},
	}

	return flags.bindLaneWriter(cmd)
}

func newTaskBlockCommand() *cobra.Command {
	var flags storeFlags
	var notes string

	cmd := &cobra.Command{
		Use:   "block <id>",
		Short: "Mark a task blocked",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			task, commit, err := runLaneWrite(cmd, &flags, func(ctx context.Context, lanes *memory.Lanes) (memory.Task, string, error) {
				return lanes.BlockTask(ctx, args[0], notes)
			})
			return emitLaneWrite(cmd, taskInfo{Task: task, Commit: commit, Error: laneError(err)}, commit,
				"`memdolt task list --status all` for task "+task.ID,
				[]string{fmt.Sprintf("task %s is blocked (commit %s)", task.ID, commit)}, err)
		},
	}
	cmd.Flags().StringVar(&notes, "notes", "", "what the task is blocked on; replaces the task's notes")

	return flags.bindLaneWriter(cmd)
}

func newTaskListCommand() *cobra.Command {
	var flags storeFlags
	var status string

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List tasks, open ones by default",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return flags.run(cmd, func(ctx context.Context, lanes *memory.Lanes) error {
				tasks, err := lanes.Tasks(ctx, status)
				if err != nil {
					return err
				}
				lines := make([]string, 0, len(tasks))
				for _, task := range tasks {
					lines = append(lines, fmt.Sprintf("%s  %-7s  %s", task.ID, task.Status, task.Title))
				}
				if len(lines) == 0 {
					if status == "" || status == memory.StatusAny {
						lines = []string{"no tasks"}
					} else {
						lines = []string{"no " + status + " tasks"}
					}
				}
				if tasks == nil {
					tasks = []memory.Task{}
				}
				return emit(cmd, struct {
					Tasks []memory.Task `json:"tasks"`
				}{tasks}, lines)
			})
		},
	}
	cmd.Flags().StringVar(&status, "status", memory.StatusOpen,
		"which tasks to list: "+strings.Join(append(memory.TaskStatuses, memory.StatusAny), ", "))

	return flags.bind(cmd)
}

// noteInfo is what logging a session note prints.
type noteInfo struct {
	memory.Note
	Commit string `json:"commit"`
	Error  string `json:"error,omitempty"`
}

func newNoteCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "note",
		Short: "Record and list session notes",
		Long: "Session notes are a direct lane (PRD §3.1): a note commits straight to main,\n" +
			"attributed to the actor that wrote it.",
	}
	cmd.AddCommand(newNoteAddCommand(), newNoteListCommand())
	return cmd
}

func newNoteAddCommand() *cobra.Command {
	var flags storeFlags

	cmd := &cobra.Command{
		Use:   "add [text]",
		Short: "Record a session note",
		Long:  "Record a session note. With no argument the note is read from standard input.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			body, err := bodyArg(cmd, args)
			if err != nil {
				return err
			}
			note, commit, err := runLaneWrite(cmd, &flags, func(ctx context.Context, lanes *memory.Lanes) (memory.Note, string, error) {
				return lanes.LogNote(ctx, body)
			})
			return emitLaneWrite(cmd, noteInfo{Note: note, Commit: commit, Error: laneError(err)}, commit,
				"`memdolt note list` and Dolt history for note "+note.ID,
				[]string{fmt.Sprintf("noted %s as %s (commit %s)", note.ID, note.Actor, commit)}, err)
		},
	}

	return flags.bindLaneWriter(cmd)
}

func newNoteListCommand() *cobra.Command {
	var flags storeFlags
	var limit int
	var actor string
	var sinceDays int64

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List committed session notes, newest first",
		Long: "List committed main notes by creation time and id, newest first. --actor\n" +
			"matches the exact stored actor, without normalization. --since-days uses\n" +
			"a rolling UTC horizon; 0 means since the current second. Both filters\n" +
			"combine before the positive limit. Reads never flush queued MCP notes.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var actorFilter *string
			var daysFilter *int64
			if cmd.Flags().Changed("actor") {
				actorFilter = &actor
			}
			if cmd.Flags().Changed("since-days") {
				daysFilter = &sinceDays
			}
			return flags.runLaneRead(cmd, func(ctx context.Context, lanes *memory.Lanes) error {
				notes, err := lanes.NotesFiltered(ctx, limit, actorFilter, daysFilter)
				if err != nil {
					return err
				}
				lines := make([]string, 0, len(notes))
				for _, note := range notes {
					// A note is free text and may span lines; a listing is
					// one line per note, so its whitespace is collapsed.
					lines = append(lines, fmt.Sprintf("%s  %s  %s",
						stamp(note.CreatedAt), note.Actor, strings.Join(strings.Fields(note.Text), " ")))
				}
				if len(lines) == 0 {
					lines = []string{"no session notes"}
				}
				if notes == nil {
					notes = []memory.Note{}
				}
				return emit(cmd, struct {
					Notes []memory.Note `json:"notes"`
				}{notes}, lines)
			})
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 25, fmt.Sprintf("how many notes to list (1 through %d)", store.DefaultMaxRows))
	cmd.Flags().StringVar(&actor, "actor", "", "exact stored actor, for example agent:codex")
	cmd.Flags().Int64Var(&sinceDays, "since-days", 0, "include notes from the last DAYS days (0 through 106751)")

	return flags.bind(cmd)
}

// commandInfo is what recording a command prints.
type commandInfo struct {
	memory.Command
	Commit string `json:"commit"`
	Error  string `json:"error,omitempty"`
}

// commandLine renders a recorded command as one human-readable line.
func commandLine(command memory.Command) string {
	line, run, exit, success, failure := "unknown", "unknown", "unknown", "unknown", "unknown"
	if command.Cmdline != nil {
		line = *command.Cmdline
	}
	if command.LastRunAt != nil {
		run = stamp(*command.LastRunAt)
	}
	if command.LastExitCode != nil {
		exit = strconv.Itoa(*command.LastExitCode)
	}
	if command.SuccessCount != nil {
		success = strconv.Itoa(*command.SuccessCount)
	}
	if command.FailCount != nil {
		failure = strconv.Itoa(*command.FailCount)
	}
	return fmt.Sprintf("%s: %s (last run %s, exit %s; %s ok, %s failed)", command.Kind, line, run, exit, success, failure)
}

func newCommandCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "command",
		Short: "Record and look up the project's build, test, run and lint commands",
		Long: "Recorded commands are a direct lane (PRD §3.1), keyed by kind: there is one\n" +
			"build command and one test command per project, so recording replaces the\n" +
			"stored command line and adds a tick to its success or failure tally.",
	}
	cmd.AddCommand(newCommandRecordCommand("record"), newCommandRecordCommand("verify"), newCommandGetCommand(), newCommandListCommand())
	return cmd
}

func newCommandRecordCommand(verb string) *cobra.Command {
	var flags storeFlags
	var exitCode int

	cmd := &cobra.Command{
		Use:   verb + " <kind> <cmdline>",
		Short: "Record how a command of one kind ran",
		Long: "Record an observed command line and exit status; this does not execute\n" +
			"the command. Update its kind's success/failure counters in one attributed\n" +
			"memory commit. verify requires an explicit --exit-code; record keeps --exit.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			command, commit, err := runLaneWrite(cmd, &flags, func(ctx context.Context, lanes *memory.Lanes) (memory.Command, string, error) {
				return lanes.RecordCommand(ctx, args[0], args[1], exitCode)
			})
			return emitLaneWrite(cmd, commandInfo{Command: command, Commit: commit, Error: laneError(err)}, commit,
				"`memdolt command get "+args[0]+"` and Dolt history",
				[]string{fmt.Sprintf("recorded %s (commit %s)", commandLine(command), commit)}, err)
		},
	}
	exitFlag := "exit"
	if verb == "verify" {
		exitFlag = "exit-code"
	}
	cmd.Flags().IntVar(&exitCode, exitFlag, 0, "the exit status the run finished with")
	if verb == "verify" {
		_ = cmd.MarkFlagRequired(exitFlag)
	}

	return flags.bindLaneWriter(cmd)
}

func newCommandGetCommand() *cobra.Command {
	var flags storeFlags

	cmd := &cobra.Command{
		Use:   "get <kind>",
		Short: "Show the recorded command of one kind",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return flags.runLaneRead(cmd, func(ctx context.Context, lanes *memory.Lanes) error {
				command, err := lanes.Command(ctx, args[0])
				if err != nil {
					return err
				}
				return emit(cmd, command, []string{commandLine(command)})
			})
		},
	}

	return flags.bind(cmd)
}

func newCommandListCommand() *cobra.Command {
	var flags storeFlags
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List committed command kinds, newest run first",
		Long: "List all recorded kinds at committed main, newest run first, with the\n" +
			"schema's kind order breaking ties: build, test, run, lint, other. Includes\n" +
			"command line, last exit/time and success/failure counters.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return flags.runLaneRead(cmd, func(ctx context.Context, lanes *memory.Lanes) error {
				commands, err := lanes.Commands(ctx)
				if err != nil {
					return err
				}
				lines := make([]string, 0, len(commands))
				for _, command := range commands {
					lines = append(lines, commandLine(command))
				}
				if len(lines) == 0 {
					lines = []string{"no recorded commands"}
				}
				return emit(cmd, struct {
					Commands []memory.Command `json:"commands"`
				}{commands}, lines)
			})
		},
	}
	return flags.bind(cmd)
}

// narrativeInfo is what setting a narrative prints.
type narrativeInfo struct {
	memory.Narrative
	Commit string `json:"commit"`
	Error  string `json:"error,omitempty"`
}

// newNarrativeCommand builds `memdolt state` or `memdolt arch`. The two
// narratives are one command shape over two tables (§6.1), so they are
// built from one constructor rather than kept in step by hand.
func newNarrativeCommand(kind memory.NarrativeKind, subject string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   string(kind),
		Short: "Read and rewrite the project's " + subject,
		Long: "The " + subject + " narrative is a direct lane (PRD §3.1). Setting it appends a\n" +
			"new version rather than overwriting the old one, because the history of how\n" +
			"the project described itself is a product feature (§3.2): dolt_log over the\n" +
			"narrative table is that timeline. show returns the newest committed version;\n" +
			"history lists the appended versions still present at committed main.",
	}
	cmd.AddCommand(newNarrativeSetCommand(kind, subject), newNarrativeShowCommand(kind, subject), newNarrativeHistoryCommand(kind, subject))
	return cmd
}

func newNarrativeSetCommand(kind memory.NarrativeKind, subject string) *cobra.Command {
	var flags storeFlags

	cmd := &cobra.Command{
		Use:   "set [body]",
		Short: "Record a new version of the " + subject,
		Long: "Record a new version of the " + subject + " narrative. With no argument the\n" +
			"body is read from standard input.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			body, err := bodyArg(cmd, args)
			if err != nil {
				return err
			}
			narrative, commit, err := runLaneWrite(cmd, &flags, func(ctx context.Context, lanes *memory.Lanes) (memory.Narrative, string, error) {
				return lanes.SetNarrative(ctx, kind, body)
			})
			return emitLaneWrite(cmd, narrativeInfo{Narrative: narrative, Commit: commit, Error: laneError(err)}, commit,
				"`memdolt "+string(kind)+" show` and Dolt history for narrative "+narrative.ID,
				[]string{fmt.Sprintf("recorded %s %s (commit %s)", kind, narrative.ID, commit)}, err)
		},
	}

	return flags.bindLaneWriter(cmd)
}

func newNarrativeShowCommand(kind memory.NarrativeKind, subject string) *cobra.Command {
	var flags storeFlags

	cmd := &cobra.Command{
		Use:   "show",
		Short: "Print the current " + subject,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return flags.runLaneRead(cmd, func(ctx context.Context, lanes *memory.Lanes) error {
				narrative, err := lanes.Narrative(ctx, kind)
				if err != nil {
					return err
				}
				// The body alone, so that it can be redirected to a file
				// without a header to strip; --json carries the rest.
				return emit(cmd, narrative, []string{narrative.Body})
			})
		},
	}

	return flags.bind(cmd)
}

func newNarrativeHistoryCommand(kind memory.NarrativeKind, subject string) *cobra.Command {
	var flags storeFlags
	var limit int
	cmd := &cobra.Command{
		Use:   "history",
		Short: "List committed versions of the " + subject + ", newest first",
		Long: "List appended narrative rows at committed main by creation time and id,\n" +
			"newest first. Include each full body, id, canonical/raw actor and creation\n" +
			"time for the appended versions still present in the narrative table.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return flags.runLaneRead(cmd, func(ctx context.Context, lanes *memory.Lanes) error {
				history, err := lanes.NarrativeHistory(ctx, kind, limit)
				if err != nil {
					return err
				}
				lines := make([]string, 0, len(history))
				for _, narrative := range history {
					lines = append(lines, fmt.Sprintf("%s  %s  %s (raw %q)\n%s",
						stamp(narrative.CreatedAt), narrative.ID, narrative.Actor, narrative.ActorRaw, narrative.Body))
				}
				if len(lines) == 0 {
					lines = []string{"no " + string(kind) + " history"}
				}
				return emit(cmd, struct {
					History []memory.Narrative `json:"history"`
				}{history}, lines)
			})
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 25, fmt.Sprintf("how many versions to list (1 through %d)", store.DefaultMaxRows))
	return flags.bind(cmd)
}
