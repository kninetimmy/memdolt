package main

import (
	"errors"
	"fmt"

	"github.com/kninetimmy/memdolt/internal/codeindex"
	"github.com/spf13/cobra"
)

func newIngestGitCommand() *cobra.Command {
	var dir, since string
	cmd := &cobra.Command{
		Use: "ingest-git", Short: "Cache local committed Git file history in the derived code index",
		Long: "Read locally available commits ending at one captured HEAD, or --since <commit-ish>..HEAD.\n" +
			"This is a revision range, not a date. Maximum 100000 commits and 64 MiB per Git response.\n" +
			"Author dates order history; rename/copy records use destinations; merges compare the first parent.\n" +
			"Repeated ingestion is idempotent. No source bodies, Dolt, models, memory commits or sync.\n" +
			"code rm removes cached history too; code index preserves it and upgrades schema v1 in place.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var from *string
			if cmd.Flags().Changed("since") {
				from = &since
			}
			summary, err := codeindex.IngestGit(cmd.Context(), dir, from)
			if err != nil && !summary.Committed && !summary.OutcomeUnknown {
				return err
			}
			out := struct {
				codeindex.GitIngestSummary
				Error string `json:"error,omitempty"`
			}{GitIngestSummary: summary, Error: laneError(err)}
			rangeText := summary.Head
			if summary.Since != nil {
				rangeText = *summary.Since + ".." + rangeText
			}
			lines := []string{fmt.Sprintf("Git history range %s (shallow=%t): committed=%t, outcomeUnknown=%t; %d commits seen, %d indexed; %d unique files, %d links; %d denied commits, %d denied file changes.",
				rangeText, summary.Shallow, summary.Committed, summary.OutcomeUnknown, summary.CommitsSeen, summary.CommitsIndexed, summary.UniqueFilesSeen, summary.CommitFileLinksSeen, summary.DeniedCommits, summary.DeniedFilesSkipped)}
			if err != nil {
				lines = append(lines, "error: "+err.Error())
			}
			return errors.Join(err, emit(cmd, out, lines))
		},
	}
	cmd.Flags().StringVar(&dir, "dir", ".", "repository directory")
	cmd.Flags().StringVar(&since, "since", "", "exclusive commit-ish lower boundary; only its difference from captured HEAD is ingested")
	return cmd
}
