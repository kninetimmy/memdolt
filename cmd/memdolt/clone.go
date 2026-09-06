package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

func newCloneCommand() *cobra.Command {
	var dir, user string
	cmd := &cobra.Command{
		Use:   "clone <remote-url>",
		Short: "Clone an existing main-branch memdolt store into this repository",
		Long: "Clone committed main and its history into <dir>/.memdolt/dolt and register\n" +
			"origin. Accepts explicit absolute http:// or https:// remotesapi URLs and\n" +
			"absolute file:/// URLs (file:///C:/path on Windows). URL userinfo, queries,\n" +
			"fragments, and other schemes are refused. --user accepts 1-32 ASCII letters,\n" +
			"digits, dots, underscores, or hyphens; its password must be supplied only\n" +
			"through the DOLT_REMOTE_PASSWORD environment variable. Without --user the\n" +
			"request is anonymous. Use your private network for remotesapi access.\n\n" +
			"Stop any store owner first. Existing databases and nonempty data directories\n" +
			"are refused before transfer; adjacent configuration and indexes are preserved.\n" +
			"Destination paths containing ? or % cannot round-trip through the pinned\n" +
			"embedded path parsers and are refused.\n" +
			"Failed artifacts are retained at the reported path: inspect them and choose\n" +
			"a fresh destination to retry. Older schemas require explicit `memdolt init`;\n" +
			"newer schemas require a newer binary. Clone never initializes or migrates\n" +
			"memory. Success is printed only after validation and close. Progress is\n" +
			"suppressed, including in --json mode. Push/pull and remote status are deferred.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.Flags().Changed("user") && user == "" {
				return errors.New("--user must not be empty; omit it for anonymous access")
			}
			info, err := localdolt.Clone(cmd.Context(), localdolt.Config{BaseDir: dir, Actor: cliActor}, args[0], user)
			if err != nil {
				return err
			}
			return emit(cmd, info, []string{
				"cloned memdolt store at " + info.Store,
				"main: " + info.MainCommit,
				fmt.Sprintf("schema: v%d", info.SchemaVersion),
			})
		},
	}
	cmd.Flags().StringVar(&dir, "dir", ".", "repository root to clone beneath")
	cmd.Flags().StringVar(&user, "user", "", "remote SQL user (password from DOLT_REMOTE_PASSWORD only)")
	return cmd
}
