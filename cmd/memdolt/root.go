package main

import "github.com/spf13/cobra"

// jsonOutput backs the --json persistent flag shared by every memdolt
// command (the memhub convention, PRD §14): when set, a command emits a
// single machine-readable JSON object on stdout instead of human-readable
// text.
var jsonOutput bool

// newRootCommand builds the memdolt root command and wires up its
// subcommands. It is a constructor (rather than a package-level var) so
// tests can build a fresh command tree with fresh flag state.
func newRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:           "memdolt",
		Short:         "memdolt gives coding agents durable, git-native project memory backed by Dolt",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.PersistentFlags().BoolVar(&jsonOutput, "json", false,
		"emit machine-readable JSON on stdout instead of human-readable text")

	root.AddCommand(newVersionCommand())

	return root
}
