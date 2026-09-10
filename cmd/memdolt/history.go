package main

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

func newHistoryCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use: "history", Short: "Inspect committed memory changes, values and native blame",
		Long: "Read fact/decision row history by explicit id, or the state/arch table timeline.\n" +
			"Each change includes native commit and parent evidence and complete nullable\n" +
			"before/after images. Native author, committer and dates are separate from\n" +
			"stored source, actor and timestamps. Changes follow native log order, then\n" +
			"parent order and row id; --limit bounds change rows (default 25).\n\n" +
			"--as-of selects a full Dolt commit hash verified in captured main ancestry.\n" +
			"Current values and blame describe that revision; state/arch select its newest\n" +
			"row by stored creation time then id, while changes cover the whole table.\n" +
			"state history / arch history still list appended versions present at main.\n" +
			"Use --json for the shared MCP/owner result, including captured main/revision,\n" +
			"null for absent rows/blame and [] for empty history. Reads require existing\n" +
			"initialized repository memory and never flush queued MCP notes.",
	}
	for _, subject := range []string{"fact", "decision", "state", "arch"} {
		var flags storeFlags
		var asOf string
		opts := localdolt.HistoryOptions{Subject: subject}
		child := &cobra.Command{
			Use: subject, Short: "Inspect committed " + subject + " history", Long: cmd.Long, Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, args []string) error {
				if len(args) == 1 {
					opts.ID = &args[0]
				}
				if cmd.Flags().Changed("as-of") {
					opts.AsOf = &asOf
				}
				if err := opts.Validate(); err != nil {
					return err
				}
				if err := localdolt.RequireExistingTransferStore(flags.dir); err != nil {
					return err
				}
				st, err := flags.open(cmd.Context(), cliActor)
				if err != nil {
					return err
				}
				result, err := st.History(cmd.Context(), opts)
				if err = errors.Join(err, st.Close()); err != nil {
					return err
				}
				lines := []string{"history: " + result.Subject, "captured main: " + result.MainCommit, "selected revision: " + result.Revision}
				for _, field := range []struct {
					name  string
					value any
				}{{"current", result.Current}, {"native blame", result.Blame}} {
					encoded, err := json.Marshal(field.value)
					if err != nil {
						return err
					}
					lines = append(lines, field.name+": "+string(encoded))
				}
				if len(result.Changes) == 0 {
					lines = append(lines, "no changes")
				}
				for _, change := range result.Changes {
					encoded, err := json.Marshal(change)
					if err != nil {
						return err
					}
					lines = append(lines, string(encoded))
				}
				return emit(cmd, result, lines)
			},
		}
		if subject == "fact" || subject == "decision" {
			child.Use, child.Args = subject+" <id>", cobra.ExactArgs(1)
		}
		child.Flags().IntVar(&opts.Limit, "limit", 25, fmt.Sprintf("maximum change rows (1 through %d)", store.DefaultMaxRows))
		child.Flags().StringVar(&asOf, "as-of", "", "full Dolt commit hash in captured main ancestry")
		cmd.AddCommand(flags.bind(child))
	}
	return cmd
}
