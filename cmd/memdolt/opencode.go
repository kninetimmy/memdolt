package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/kninetimmy/memdolt/internal/memory"
	opencodehost "github.com/kninetimmy/memdolt/internal/opencode"
)

var openCodeCLIActor = memory.Actor{Name: memory.AgentPrefix + "opencode", Raw: "cli"}

type openCodeSessionVerifier func(context.Context, string) (opencodehost.SessionInfo, error)

func newOpenCodeCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "opencode",
		Short: "Verify OpenCode host context for workflow integration",
	}
	cmd.AddCommand(newOpenCodeSessionInfoCommand(), newOpenCodeWrapUpNoteCommand())
	return cmd
}

func newOpenCodeSessionInfoCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "session-info <current-session-id>",
		Short: "Verify and report the current OpenCode Session.Info",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			info, err := opencodehost.VerifySession(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return emit(cmd, info, []string{"verified OpenCode session " + info.SessionID})
		},
	}
}

func newOpenCodeWrapUpNoteCommand() *cobra.Command {
	var flags storeFlags
	cmd := &cobra.Command{
		Use:   "wrap-up-note <current-session-id> [text]",
		Short: "Verify OpenCode identity, then record the wrap-up note",
		Long: "Verify the host-supplied current OpenCode session id before opening the store,\n" +
			"then record one note with exact Session.Info provenance. With no text argument,\n" +
			"the note is read from standard input.",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			body, err := bodyArg(cmd, args[1:])
			if err != nil {
				return err
			}
			note, err := logOpenCodeWrapUpNote(
				cmd.Context(), flags.dir, args[0], body, opencodehost.VerifySession,
			)
			if err != nil {
				return err
			}
			return emit(cmd, note,
				[]string{fmt.Sprintf("noted %s as %s (commit %s)", note.ID, note.Actor, note.Commit)})
		},
	}
	return flags.bind(cmd)
}

// logOpenCodeWrapUpNote verifies before openCommandStore, making unavailable,
// malformed, missing, or mismatched identity a no-store-open and no-write path.
func logOpenCodeWrapUpNote(
	ctx context.Context,
	dir, sessionID, body string,
	verify openCodeSessionVerifier,
) (result noteInfo, err error) {
	info, err := verify(ctx, sessionID)
	if err != nil {
		return noteInfo{}, err
	}

	st, err := openCommandStore(ctx, dir, openCodeCLIActor.CommitAuthor())
	if err != nil {
		return noteInfo{}, err
	}
	defer func() { err = errors.Join(err, st.Close()) }()
	if err := requireCurrentSchema(ctx, st); err != nil {
		return noteInfo{}, err
	}

	note, commit, err := memory.New(st, openCodeCLIActor).LogNoteWithProvenance(ctx, body, memory.NoteProvenance{
		SessionID:  info.SessionID,
		AgentID:    info.AgentID,
		ProviderID: info.ProviderID,
		ModelID:    info.ModelID,
		Variant:    info.Variant,
	})
	if err != nil {
		return noteInfo{}, err
	}
	return noteInfo{Note: note, Commit: commit}, nil
}
