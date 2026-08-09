package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/kninetimmy/memdolt/internal/memory"
	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
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
	dir   string
	actor string
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

// run opens the store the flags name, hands its direct lanes to fn and
// closes it again.
func (f *storeFlags) run(cmd *cobra.Command, fn func(context.Context, *memory.Lanes) error) (err error) {
	actor, err := memory.NormalizeActor(f.actor)
	if err != nil {
		return err
	}

	st, err := localdolt.New(localdolt.Config{BaseDir: f.dir, Actor: actor.CommitAuthor()})
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	if err := st.Open(ctx); err != nil {
		return err
	}
	// The store holds the single-owner lock (PRD §5.2) until it is closed,
	// and closing is itself a write path, so its failure is reported rather
	// than swallowed.
	defer func() { err = errors.Join(err, st.Close()) }()

	if err := requireCurrentSchema(ctx, st); err != nil {
		return err
	}
	return fn(ctx, memory.New(st, actor))
}

// requireCurrentSchema refuses a store the shipped statements would not
// fit. Open already turns away a store newer than this binary (§6.4); this
// is the other direction, where the tables a lane writes may not exist
// yet, and it names the command that fixes it rather than letting the
// write fail as "table not found".
func requireCurrentSchema(ctx context.Context, st *localdolt.Store) error {
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
			return flags.run(cmd, func(ctx context.Context, lanes *memory.Lanes) error {
				task, commit, err := lanes.AddTask(ctx, args[0], notes)
				if err != nil {
					return err
				}
				return emit(cmd, taskInfo{Task: task, Commit: commit},
					[]string{fmt.Sprintf("opened task %s (commit %s)", task.ID, commit)})
			})
		},
	}
	cmd.Flags().StringVar(&notes, "notes", "", "detail to record alongside the title")

	return flags.bindWriter(cmd)
}

func newTaskDoneCommand() *cobra.Command {
	var flags storeFlags

	cmd := &cobra.Command{
		Use:   "done <id>",
		Short: "Mark a task done",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return flags.run(cmd, func(ctx context.Context, lanes *memory.Lanes) error {
				task, commit, err := lanes.CompleteTask(ctx, args[0])
				if err != nil {
					return err
				}
				return emit(cmd, taskInfo{Task: task, Commit: commit},
					[]string{fmt.Sprintf("task %s is done (commit %s)", task.ID, commit)})
			})
		},
	}

	return flags.bindWriter(cmd)
}

func newTaskBlockCommand() *cobra.Command {
	var flags storeFlags
	var notes string

	cmd := &cobra.Command{
		Use:   "block <id>",
		Short: "Mark a task blocked",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return flags.run(cmd, func(ctx context.Context, lanes *memory.Lanes) error {
				task, commit, err := lanes.BlockTask(ctx, args[0], notes)
				if err != nil {
					return err
				}
				return emit(cmd, taskInfo{Task: task, Commit: commit},
					[]string{fmt.Sprintf("task %s is blocked (commit %s)", task.ID, commit)})
			})
		},
	}
	cmd.Flags().StringVar(&notes, "notes", "", "what the task is blocked on; replaces the task's notes")

	return flags.bindWriter(cmd)
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
			return flags.run(cmd, func(ctx context.Context, lanes *memory.Lanes) error {
				note, commit, err := lanes.LogNote(ctx, body)
				if err != nil {
					return err
				}
				return emit(cmd, noteInfo{Note: note, Commit: commit},
					[]string{fmt.Sprintf("noted %s as %s (commit %s)", note.ID, note.Actor, commit)})
			})
		},
	}

	return flags.bindWriter(cmd)
}

func newNoteListCommand() *cobra.Command {
	var flags storeFlags
	var limit int

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List recent session notes, newest first",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return flags.run(cmd, func(ctx context.Context, lanes *memory.Lanes) error {
				notes, err := lanes.Notes(ctx, limit)
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
	cmd.Flags().IntVar(&limit, "limit", 20, "how many notes to list")

	return flags.bind(cmd)
}

// commandInfo is what recording a command prints.
type commandInfo struct {
	memory.Command
	Commit string `json:"commit"`
}

// commandLine renders a recorded command as one human-readable line.
func commandLine(command memory.Command) string {
	return fmt.Sprintf("%s: %s (last run %s, exit %d; %d ok, %d failed)",
		command.Kind, command.Cmdline, stamp(command.LastRunAt), command.LastExitCode,
		command.SuccessCount, command.FailCount)
}

func newCommandCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "command",
		Short: "Record and look up the project's build, test, run and lint commands",
		Long: "Recorded commands are a direct lane (PRD §3.1), keyed by kind: there is one\n" +
			"build command and one test command per project, so recording replaces the\n" +
			"stored command line and adds a tick to its success or failure tally.",
	}
	cmd.AddCommand(newCommandRecordCommand(), newCommandGetCommand())
	return cmd
}

func newCommandRecordCommand() *cobra.Command {
	var flags storeFlags
	var exitCode int

	cmd := &cobra.Command{
		Use:   "record <kind> <cmdline>",
		Short: "Record how a command of one kind ran",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return flags.run(cmd, func(ctx context.Context, lanes *memory.Lanes) error {
				command, commit, err := lanes.RecordCommand(ctx, args[0], args[1], exitCode)
				if err != nil {
					return err
				}
				return emit(cmd, commandInfo{Command: command, Commit: commit},
					[]string{fmt.Sprintf("recorded %s (commit %s)", commandLine(command), commit)})
			})
		},
	}
	cmd.Flags().IntVar(&exitCode, "exit", 0, "the exit status the run finished with")

	return flags.bindWriter(cmd)
}

func newCommandGetCommand() *cobra.Command {
	var flags storeFlags

	cmd := &cobra.Command{
		Use:   "get <kind>",
		Short: "Show the recorded command of one kind",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return flags.run(cmd, func(ctx context.Context, lanes *memory.Lanes) error {
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

// narrativeInfo is what setting a narrative prints.
type narrativeInfo struct {
	memory.Narrative
	Commit string `json:"commit"`
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
			"narrative table is that timeline. Reading it returns the newest version.",
	}
	cmd.AddCommand(newNarrativeSetCommand(kind, subject), newNarrativeShowCommand(kind, subject))
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
			return flags.run(cmd, func(ctx context.Context, lanes *memory.Lanes) error {
				narrative, commit, err := lanes.SetNarrative(ctx, kind, body)
				if err != nil {
					return err
				}
				return emit(cmd, narrativeInfo{Narrative: narrative, Commit: commit},
					[]string{fmt.Sprintf("recorded %s %s (commit %s)", kind, narrative.ID, commit)})
			})
		},
	}

	return flags.bindWriter(cmd)
}

func newNarrativeShowCommand(kind memory.NarrativeKind, subject string) *cobra.Command {
	var flags storeFlags

	cmd := &cobra.Command{
		Use:   "show",
		Short: "Print the current " + subject,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return flags.run(cmd, func(ctx context.Context, lanes *memory.Lanes) error {
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
