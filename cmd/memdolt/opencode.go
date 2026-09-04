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
		Short: "Verify supplied OpenCode session IDs against the API",
	}
	cmd.AddCommand(newOpenCodeSessionInfoCommand(), newOpenCodeWrapUpNoteCommand())
	return cmd
}

func newOpenCodeSessionInfoCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "session-info <current-session-id>",
		Short: "Verify and report Session.Info for the supplied ID",
		Long: "Match the supplied ID against the OpenCode API's Session.Info. Workflows must\n" +
			"obtain the current ID from host context, never discover or guess another session.\n" +
			"The CLI cannot authenticate the origin of the supplied ID.",
		Args: cobra.ExactArgs(1),
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
		Short: "Verify the supplied OpenCode session ID, then record the wrap-up note",
		Long: "Verify the supplied OpenCode session id against its API before opening the store,\n" +
			"then record one note with exact Session.Info provenance. With no text argument,\n" +
			"the note is read from standard input.\n\n" +
			"Workflows must obtain the current ID from OpenCode host context, never discover\n" +
			"or guess another session. The CLI verifies the supplied ID against the API;\n" +
			"it cannot authenticate the origin of that ID.",
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
