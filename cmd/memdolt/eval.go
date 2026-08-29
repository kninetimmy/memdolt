package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/kninetimmy/memdolt/internal/embedding"
	"github.com/kninetimmy/memdolt/internal/layout"
	"github.com/kninetimmy/memdolt/internal/memory"
	"github.com/kninetimmy/memdolt/internal/retrieval"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

func newEvalCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "eval",
		Short: "Evaluate production retrieval against committed golden queries",
	}
	cmd.AddCommand(newEvalRetrievalCommand())
	return cmd
}

func newEvalRetrievalCommand() *cobra.Command {
	var flags storeFlags
	var goldenPath, mode string
	cmd := &cobra.Command{
		Use:   "retrieval",
		Short: "Report Recall@3 and empty-query safety against a golden file",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			selectedMode, err := retrieval.ParseMode(mode)
			if err != nil {
				return err
			}
			path := goldenPath
			if path == "" {
				path = retrieval.DefaultGoldenPath
			}
			if !filepath.IsAbs(path) {
				path = filepath.Join(flags.dir, path)
			}
			golden, err := retrieval.LoadGolden(path)
			if err != nil {
				return err
			}

			return flags.runStore(cmd, func(ctx context.Context, st *localdolt.Store, _ memory.Actor) error {
				paths, err := layout.New(flags.dir)
				if err != nil {
					return err
				}
				cfg, err := retrieval.LoadConfig(paths.ConfigFile())
				if err != nil {
					return err
				}
				var engine *embedding.Engine
				if selectedMode == retrieval.ModeHybrid {
					engine, err = embedding.Open(ctx, embedding.Options{})
					if err != nil {
						return err
					}
				}
				summary, evalErr := retrieval.EvaluateGolden(ctx, st, paths.EmbeddingsFile(), engine, cfg, golden,
					retrieval.EvalOptions{GoldenPath: path, Mode: selectedMode})
				if engine != nil {
					evalErr = errors.Join(evalErr, engine.Close())
				}
				if evalErr != nil && !errors.Is(evalErr, retrieval.ErrBelowBaseline) {
					return evalErr
				}
				if err := emit(cmd, summary, evalLines(summary)); err != nil {
					return errors.Join(evalErr, err)
				}
				return evalErr
			})
		},
	}
	cmd.Flags().StringVar(&goldenPath, "golden", "", "golden JSON path relative to --dir (default tests/golden/retrieval_golden.json)")
	cmd.Flags().StringVar(&mode, "mode", string(retrieval.ModeHybrid), "production retrieval mode to evaluate: fts or hybrid")
	return flags.bind(cmd)
}

func evalLines(summary retrieval.EvalSummary) []string {
	lines := []string{
		fmt.Sprintf("memdolt eval retrieval — %s (%d queries)", summary.GoldenPath, summary.Totals.Queries),
		fmt.Sprintf("Mode: %s | K: %d | Elapsed: %d ms", summary.Mode, summary.K, summary.ElapsedMS),
		fmt.Sprintf("Recall@%d: %d/%d = %.1f%%", summary.K, summary.Totals.MatchPasses,
			summary.Totals.MatchQueries, summary.RecallAtK*100),
		fmt.Sprintf("Safety: %d/%d empty-query probes returned no results (%d failures)",
			summary.Totals.EmptyPasses, summary.Totals.EmptyQueries, summary.Totals.SafetyFailures),
		"Per-query outcomes:",
	}
	for _, outcome := range summary.Outcomes {
		status := "PASS"
		if !outcome.Passed {
			status = "FAIL"
		}
		detail := fmt.Sprintf("%d hit(s) returned", outcome.ReturnedCount)
		if outcome.MatchedRank != nil {
			detail = fmt.Sprintf("rank %d, score %.3f", *outcome.MatchedRank, *outcome.MatchedScore)
		}
		lines = append(lines, fmt.Sprintf("  [%s] %s (%s) — %s", status, outcome.ID, outcome.Kind, detail))
		if outcome.FailureReason != nil {
			lines = append(lines, "        "+*outcome.FailureReason)
		}
	}
	return lines
}
