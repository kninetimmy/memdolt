package main

import (
	"github.com/spf13/cobra"

	"github.com/kninetimmy/memdolt/internal/memory"
)

// jsonOutput backs the --json persistent flag shared by every memdolt
// command (the memhub convention, PRD §14): when set, a command emits a
// single machine-readable JSON object on stdout instead of human-readable
// text.
var jsonOutput bool

// cliActor is the identity a CLI invocation opens the store under, and
// the one its commits are attributed to. A CLI is a person running a
// command, so it is PRD §3.1's plain "user" rather than an agent. The
// address is in .invalid (RFC 2606) because memdolt asserts an identity
// for dolt_log and dolt_blame, not a mailbox.
var cliActor = memory.UserActor.CommitAuthor()

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

	root.AddCommand(newDoctorCommand())
	root.AddCommand(newEvalCommand())
	root.AddCommand(newIndexCommand())
	root.AddCommand(newInitCommand())
	root.AddCommand(newRecallCommand())
	root.AddCommand(newSearchCommand())
	root.AddCommand(newServeCommand())
	root.AddCommand(newVersionCommand())

	// The direct lanes of PRD §3.1.
	root.AddCommand(newTaskCommand())
	root.AddCommand(newNoteCommand())
	root.AddCommand(newCommandCommand())
	root.AddCommand(newNarrativeCommand(memory.StateNarrative, "status narrative"))
	root.AddCommand(newNarrativeCommand(memory.ArchNarrative, "architecture narrative"))

	// The review gate of PRD §7 over the staged-write lane of §3.1.
	root.AddCommand(newReviewCommand())

	return root
}
