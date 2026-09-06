package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/kninetimmy/memdolt/internal/layout"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

type repoTableChange struct {
	Table  string `json:"table"`
	Staged bool   `json:"staged"`
	Status string `json:"status"`
}

type repoStatusReport struct {
	LocalOnly        bool              `json:"localOnly"`
	Store            string            `json:"store"`
	MainCommit       string            `json:"mainCommit"`
	SchemaVersion    int               `json:"schemaVersion"`
	Clean            bool              `json:"clean"`
	Changes          []repoTableChange `json:"changes"`
	PendingProposals struct {
		Repo   int `json:"repo"`
		Global int `json:"global"`
	} `json:"pendingProposals"`
}

func newRepoCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "repo",
		Short: "Inspect the local memory repository",
	}
	cmd.AddCommand(newRepoStatusCommand())
	return cmd
}

func newRepoStatusCommand() *cobra.Command {
	var flags storeFlags
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Report local-only main and pending-proposal status",
		Long: "Report the local store path, main commit, schema version, main working-set\n" +
			"table changes (including staged/status values), and repo/global pending counts.\n" +
			"Reads through the authenticated local owner when one is running.\n\n" +
			"This report is local-only: it makes no hub or remote request and says\n" +
			"nothing about remote configuration, reachability, or synchronization. Remote\n" +
			"status/diff, pull/push, conflict dialogs, and hub setup remain deferred.\n" +
			"It changes no memory or proposal state and never initializes a missing store.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			paths, err := layout.New(flags.dir)
			if err != nil {
				return err
			}
			// Open creates a missing database. The parent data directory alone
			// does not prove that memory exists, so check its Dolt manifest.
			manifest := filepath.Join(paths.DoltDataDir(), localdolt.DatabaseName, ".dolt", "noms", "manifest")
			info, err := os.Stat(manifest)
			if os.IsNotExist(err) {
				return fmt.Errorf("there is no memdolt store at %s yet: run `memdolt init` first", paths.DoltDataDir())
			}
			if err != nil {
				return fmt.Errorf("inspect the memdolt store at %s: %w", manifest, err)
			}
			if !info.Mode().IsRegular() || info.Size() == 0 {
				return fmt.Errorf("the memdolt store manifest at %s is not a nonempty regular file", manifest)
			}
			st, err := openCommandStore(cmd.Context(), paths.Base(), cliActor)
			if err != nil {
				return err
			}
			return runRepoStatus(cmd, st)
		},
	}
	return flags.bind(cmd)
}

// runRepoStatus owns the opened store, including reporting its close failure.
func runRepoStatus(cmd *cobra.Command, st commandStore) (err error) {
	defer func() { err = errors.Join(err, st.Close()) }()
	if err := requireCurrentSchema(cmd.Context(), st); err != nil {
		return err
	}
	report, err := readRepoStatus(cmd.Context(), st)
	if err != nil {
		return err
	}
	workingSet := "clean"
	if !report.Clean {
		workingSet = "dirty"
	}
	lines := []string{
		"local-only repository status (remote state not checked)",
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
	return emit(cmd, report, lines)
}

func readRepoStatus(ctx context.Context, st commandStore) (report repoStatusReport, err error) {
	report.LocalOnly = true
	report.Store = st.DataDir()
	report.Changes = []repoTableChange{}
	report.SchemaVersion, err = st.SchemaVersion(ctx)
	if err != nil {
		return report, err
	}
	heads, err := st.Query(ctx, "SELECT hash FROM dolt_branches WHERE name = ?", localdolt.MainBranch)
	if err != nil {
		return report, fmt.Errorf("read the main commit: %w", err)
	}
	defer func() { err = errors.Join(err, heads.Close()) }()
	for heads.Next() {
		if err := heads.Scan(&report.MainCommit); err != nil {
			return report, fmt.Errorf("read the main commit: %w", err)
		}
	}
	if err := heads.Err(); err != nil {
		return report, fmt.Errorf("read the main commit: %w", err)
	}
	if report.MainCommit == "" {
		return report, errors.New("the memdolt store has no main branch")
	}
	// A revision-qualified database reads main's working set without checking
	// out a branch or relying on the pooled connection's current branch.
	changes, err := st.Query(ctx,
		"SELECT table_name, staged, status FROM `memory/main`.dolt_status ORDER BY table_name, staged, status")
	if err != nil {
		return report, fmt.Errorf("read the main working set: %w", err)
	}
	defer func() { err = errors.Join(err, changes.Close()) }()
	for changes.Next() {
		var change repoTableChange
		if err := changes.Scan(&change.Table, &change.Staged, &change.Status); err != nil {
			return report, fmt.Errorf("read the main working set: %w", err)
		}
		report.Changes = append(report.Changes, change)
	}
	if err := changes.Err(); err != nil {
		return report, fmt.Errorf("read the main working set: %w", err)
	}
	report.Clean = len(report.Changes) == 0
	proposals, err := st.PendingProposals(ctx)
	if err != nil {
		return report, fmt.Errorf("read pending proposals: %w", err)
	}
	for _, proposal := range proposals {
		switch proposal.Target {
		case localdolt.TargetRepo:
			report.PendingProposals.Repo++
		case localdolt.TargetGlobal:
			report.PendingProposals.Global++
		default:
			return report, fmt.Errorf("proposal %s has unknown target %q", proposal.ID, proposal.Target)
		}
	}
	return report, nil
}
