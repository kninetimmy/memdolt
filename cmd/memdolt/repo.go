package main

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

func newRepoCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "repo",
		Short: "Inspect the memory repository and configure remotes",
	}
	cmd.AddCommand(newRepoStatusCommand(), newRepoRemoteCommand())
	return cmd
}

func newRepoStatusCommand() *cobra.Command {
	var flags storeFlags
	var opts localdolt.RepoStatusOptions
	cmd := &cobra.Command{
		Use:   "status [remote]",
		Short: "Inspect committed local and remote main (default origin)",
		Long: "Report the local store path, committed main hash, schema version, main\n" +
			"working/staged table changes and repo/global pending counts. Default to\n" +
			"configured origin; an absent origin reports no-remote with a setup remedy.\n" +
			"An explicitly named missing remote refuses. --local retains offline status\n" +
			"and cannot be combined with a remote name, --diff or --user.\n\n" +
			"Fetch only remote main, capture exact immutable hashes and report current,\n" +
			"ahead, behind, diverged-mergeable, conflicted or diverged-unassessed. Dirty\n" +
			"divergence needs clean main before mergeability can be assessed. Schema\n" +
			"changes or unknown conflicts refuse assessment with a remedy. A clean\n" +
			"divergence preview checks data and constraints and always rolls back.\n\n" +
			"--diff shows exact committed differences FROM local main TO remote main,\n" +
			"including added/modified/deleted rows, NULLs and changed table definitions.\n" +
			"Ordinary status omits row bodies; neither view includes dirty/proposal data.\n" +
			"No diff is claimed for an absent remote or an unvalidated incoming schema.\n\n" +
			"Use the authenticated owner when running. The whole inspection coordinates\n" +
			"with memdolt mutations/transfers; foreign Dolt processes are outside this\n" +
			"boundary. Never promote main, create a commit, migrate, or change working\n" +
			"memory, proposals, tags, derived indexes, rendered files or configuration.\n" +
			"Fetched objects and the selected tracking ref may remain, even on refusal.\n" +
			"File sources are read without initialization. Fetch progress never uses\n" +
			"protocol stdout. A lost reply is not retried; inspect local main first.\n\n" +
			"URLs follow repo remote add's validated http(s)/absolute file-URL contract.\n" +
			"--user overrides the stored SQL username; omit both for anonymous access.\n" +
			"Only DOLT_REMOTE_PASSWORD in the executing owner's environment supplies a\n" +
			"password. Restart an owner to change it; passwords never cross IPC and no\n" +
			"personal Dolt credentials are loaded. Configure with repo remote add/list.\n" +
			"This inspection does not resolve divergence or complete hub/M4 acceptance.",
		Args: func(cmd *cobra.Command, args []string) error {
			if err := cobra.MaximumNArgs(1)(cmd, args); err != nil {
				return fmt.Errorf("%w; see `memdolt repo status --help`", err)
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				if args[0] == "" {
					return errors.New("remote name must not be empty; see `memdolt repo status --help`")
				}
				opts.Remote = args[0]
			}
			if cmd.Flags().Changed("user") && opts.User == "" {
				return errors.New("--user must not be empty; see `memdolt repo status --help`")
			}
			if err := opts.Validate(); err != nil {
				return err
			}
			if err := localdolt.RequireExistingTransferStore(flags.dir); err != nil {
				return err
			}
			st, err := openCommandStore(cmd.Context(), flags.dir, cliActor)
			if err != nil {
				return err
			}
			return runRepoStatus(cmd, st, opts)
		},
	}
	cmd.Flags().BoolVar(&opts.Local, "local", false, "offline status; no remote configuration read or request")
	cmd.Flags().BoolVar(&opts.Diff, "diff", false, "exact committed differences from local main to remote main")
	cmd.Flags().StringVar(&opts.User, "user", "", "SQL username override; password comes only from the owner's DOLT_REMOTE_PASSWORD")
	return flags.bind(cmd)
}

// runRepoStatus owns the opened store and closes it before emitting success.
func runRepoStatus(cmd *cobra.Command, st commandStore, opts localdolt.RepoStatusOptions) error {
	report, err := st.RepoStatus(cmd.Context(), opts)
	if err = errors.Join(err, st.Close()); err != nil {
		return err
	}
	workingSet := "clean"
	if !report.Clean {
		workingSet = "dirty"
	}
	title := "repository status: " + report.Status
	if report.Status == "offline" {
		title = "local-only repository status (remote state not checked)"
	}
	lines := []string{
		title,
		"store: " + report.Store,
		"main: " + report.MainCommit,
		fmt.Sprintf("schema: v%d", report.SchemaVersion),
		"main working set: " + workingSet,
	}
	for _, change := range report.Changes {
		lines = append(lines, fmt.Sprintf("  %s  staged=%t  %s", change.Table, change.Staged, change.Status))
	}
	lines = append(lines, fmt.Sprintf("pending proposals: %d repo, %d global",
		report.PendingProposals.Repo, report.PendingProposals.Global))
	if report.Remote != "" {
		lines = append(lines, "remote: "+report.Remote, "assessment: "+report.Assessment)
	}
	if report.RemoteCommit != "" {
		lines = append(lines, "captured remote main: "+report.RemoteCommit, "merge base: "+report.MergeBase)
	}
	for _, conflict := range report.Conflicts {
		lines = append(lines, fmt.Sprintf("  %s: %d data conflicts, %d constraint violations", conflict.Table, conflict.Data, conflict.Constraints))
	}
	if report.Remedy != "" {
		lines = append(lines, report.Remedy)
	}
	if report.Diff != nil {
		lines = append(lines, "committed diff (local-to-remote): "+report.Diff.FromCommit+" -> "+report.Diff.ToCommit)
		for _, table := range report.Diff.Tables {
			encoded, err := json.Marshal(table)
			if err != nil {
				return err
			}
			lines = append(lines, string(encoded))
		}
	}
	return emit(cmd, report, lines)
}
