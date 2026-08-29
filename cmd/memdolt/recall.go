package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kninetimmy/memdolt/internal/embedding"
	"github.com/kninetimmy/memdolt/internal/layout"
	"github.com/kninetimmy/memdolt/internal/memory"
	"github.com/kninetimmy/memdolt/internal/retrieval"
)

func newRecallCommand() *cobra.Command {
	var flags storeFlags
	var mode string
	var maxResults int
	var sourceTypes []string
	var includeStale, acceptedOnly, noRerank, provenance bool
	var minRerankScore float32

	cmd := &cobra.Command{
		Use:   "recall <query>",
		Short: "Recall ranked facts, decisions, tasks, and document chunks",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return flags.runStore(cmd, func(ctx context.Context, st commandStore, _ memory.Actor) error {
				paths, err := layout.New(flags.dir)
				if err != nil {
					return err
				}
				cfg, err := retrieval.LoadConfig(paths.ConfigFile())
				if err != nil {
					return err
				}
				options := retrieval.Options{
					Query: args[0], MaxResults: maxResults, SourceTypes: sourceTypes, Provenance: provenance,
				}
				if mode != "" {
					options.Mode, err = retrieval.ParseMode(mode)
					if err != nil {
						return err
					}
				}
				if cmd.Flags().Changed("include-stale") {
					options.IncludeStale = &includeStale
				}
				if cmd.Flags().Changed("accepted-only") {
					options.AcceptedOnly = &acceptedOnly
				}
				if noRerank {
					use := false
					options.UseReranker = &use
				}
				if cmd.Flags().Changed("min-rerank-score") {
					options.MinRerankScore = &minRerankScore
				}
				effectiveMode := options.Mode
				if effectiveMode == "" {
					effectiveMode = cfg.Mode
				}
				var engine *embedding.Engine
				if effectiveMode == retrieval.ModeHybrid {
					engine, err = embedding.Open(ctx, embedding.Options{})
					if err != nil {
						return err
					}
				}
				response, recallErr := retrieval.Recall(ctx, st, paths.EmbeddingsFile(), engine, cfg, options)
				if engine != nil {
					recallErr = errors.Join(recallErr, engine.Close())
				}
				if recallErr != nil {
					return recallErr
				}
				return emit(cmd, response, recallLines(response))
			})
		},
	}
	cmd.Flags().StringVar(&mode, "mode", "", "retrieval mode override: fts or hybrid")
	cmd.Flags().IntVar(&maxResults, "max-results", 0, "maximum returned results (0 uses retrieval.default_max_results)")
	cmd.Flags().StringSliceVar(&sourceTypes, "source-type", nil, "source type to include; repeat or comma-separate fact, decision, task, doc_chunk")
	cmd.Flags().BoolVar(&includeStale, "include-stale", false, "include and demote stale facts for this call")
	cmd.Flags().BoolVar(&acceptedOnly, "accepted-only", false, "include only user and user+agent sourced rows")
	cmd.Flags().BoolVar(&noRerank, "no-rerank", false, "disable hybrid cross-encoder reranking for this call")
	cmd.Flags().Float32Var(&minRerankScore, "min-rerank-score", 0, "override the hybrid cross-encoder score floor")
	cmd.Flags().BoolVar(&provenance, "provenance", false, "include each result's last-changing Dolt commit")
	return flags.bind(cmd)
}

func recallLines(response retrieval.Response) []string {
	lines := []string{
		fmt.Sprintf("Query: %s (mode: %s, matcher: %s, elapsed: %d ms)", response.Query, response.Mode, response.Matcher, response.ElapsedMS),
		fmt.Sprintf("Candidates: %d | Returned: %d | Available docs: %d", response.CandidateCount, response.ReturnedCount, response.AvailableDocs),
	}
	if len(response.Results) == 0 {
		lines = append(lines, "No matches.")
	}
	for _, hit := range response.Results {
		tags := ""
		if hit.Stale {
			tags += " [stale]"
		}
		if hit.SupersededBy != "" {
			tags += " [superseded by " + hit.SupersededBy + "]"
		}
		provenance := ""
		if hit.LastChanged != nil {
			provenance = fmt.Sprintf(" last_changed=%s by %s at %s", hit.LastChanged.Hash, hit.LastChanged.Author, hit.LastChanged.Date.Format("2006-01-02T15:04:05Z07:00"))
		}
		lines = append(lines,
			fmt.Sprintf("#%d [%s:%s] %s%s score=%.3f (fts=%.3f, vec=%.3f)%s",
				hit.Rank, hit.SourceType, hit.SourceID, hit.Title, tags, hit.Score, hit.FTSScore, hit.VectorScore, provenance))
		if body := strings.TrimSpace(hit.Body); body != "" {
			lines = append(lines, "    "+strings.Join(strings.Fields(body), " "))
		}
	}
	if len(response.Warnings) > 0 {
		lines = append(lines, "Warnings:")
		for _, warning := range response.Warnings {
			lines = append(lines, fmt.Sprintf("  %s (%d/%d): %s — %s", warning.Kind,
				warning.StaleCount, warning.TotalCount, warning.Reason, warning.Fix))
		}
	}
	return lines
}
