package main

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/kninetimmy/memdolt/internal/codeindex"
	"github.com/spf13/cobra"
)

func newCodeCommand() *cobra.Command {
	var dir string
	cmd := &cobra.Command{Use: "code", Short: "Manage the local tracked-source index, separate from memory embeddings",
		Long: "Code indexing uses only .memdolt/code_index.sqlite and git-tracked source.\n" +
			"It never opens Dolt or routes to its owner, and never changes recall, exports or sync.\n" +
			"Refresh preserves explicitly ingested Git history; code rm removes the whole cache, including history.\n" +
			"Grammars: Rust, C#, Java, TypeScript/TSX, JavaScript, Python and Go.\n" +
			"Secret paths, owner-credential aliases, linked and denied sources are excluded."}
	cmd.PersistentFlags().StringVar(&dir, "dir", ".", "repository directory")
	cmd.AddCommand(&cobra.Command{Use: "index", Short: "Refresh tracked files and current local code vectors", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			summary, err := codeindex.Refresh(cmd.Context(), dir, nil)
			if err != nil && !summary.Committed {
				return err
			}
			out := struct {
				codeindex.RefreshSummary
				Error string `json:"error,omitempty"`
			}{RefreshSummary: summary}
			if err != nil {
				out.Error = err.Error()
			}
			lines := []string{fmt.Sprintf("code index: %d files, %d chunks; %d new, %d changed, %d unchanged, %d deleted; %d skipped, %d excluded, %d denied, %d binary; %d embedded",
				summary.FilesTotal, summary.ChunksTotal, summary.NewFiles, summary.ChangedFiles, summary.UnchangedFiles, summary.DeletedFiles,
				summary.SkippedFiles, summary.ExcludedFiles, summary.DeniedFiles, summary.BinarySkipped, summary.EmbeddedChunks)}
			for _, skipped := range summary.Skipped {
				lines = append(lines, fmt.Sprintf("  %q: %s", skipped.Path, skipped.Reason))
			}
			return errors.Join(err, emit(cmd, out, lines))
		}})
	cmd.AddCommand(&cobra.Command{Use: "status", Short: "Inspect code-index metadata without refreshing or creating it", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			report, err := codeindex.Status(cmd.Context(), dir)
			if err != nil {
				return err
			}
			lines := []string{fmt.Sprintf("code index at %s: exists=%t, mode=%s, %d files, %d chunks, %d vectors (%d missing/invalid)",
				report.Path, report.Exists, report.Mode, report.FilesTotal, report.ChunksTotal, report.EmbeddingsTotal, report.InvalidEmbeddings)}
			if report.NeedsRebuild {
				lines = append(lines, "unsupported schema; retained for inspection with a compatible memdolt version")
			}
			if report.HeadStale {
				lines = append(lines, "indexed HEAD differs; HEAD is reporting metadata, not file freshness")
			}
			return emit(cmd, report, lines)
		}})
	cmd.AddCommand(&cobra.Command{Use: "rm", Short: "Remove the recognized code index, including cached Git history", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			result, err := codeindex.Remove(cmd.Context(), dir)
			if err != nil && !result.Removed {
				return err
			}
			return errors.Join(err, emit(cmd, result, []string{fmt.Sprintf("code index at %s: removed=%t", result.Path, result.Removed)}))
		}})
	return cmd
}

func newLocateCommand() *cobra.Command {
	var dir string
	var opts codeindex.Options
	cmd := &cobra.Command{Use: "locate <query>", Short: "Find local code by intent and return clipped path/line/symbol breadcrumbs",
		Long: "Locate lazily refreshes tracked source using millisecond mtime/size, then content hashes.\n" +
			"An edit preserving both metadata values can remain unseen. HEAD is reporting metadata only.\n" +
			"--no-refresh queries old ranking and line metadata without git commands; snippets still\n" +
			"read current files through root, link, owner-credential and deny checks. Results may be stale.\n" +
			"Snippets are at most six lines and 400 characters. Fusion and opt-in reranking have no score floor.",
		Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			opts.Query = args[0]
			result, err := codeindex.Locate(cmd.Context(), dir, nil, opts)
			if err != nil {
				return err
			}
			lines := []string{fmt.Sprintf("locate: %d/%d candidates (%s, reranked=%t)", result.ReturnedCount, result.CandidateCount, result.Mode, result.Reranked)}
			for _, hit := range result.Results {
				symbol := ""
				if hit.Symbol != nil {
					symbol = " [" + *hit.Symbol + "]"
				}
				lines = append(lines, fmt.Sprintf("%d. %s:%d-%d%s (%s, %.4f)", hit.Rank, hit.Path, hit.StartLine, hit.EndLine, symbol, hit.Kind, hit.Score), hit.Snippet)
			}
			for _, warning := range result.Warnings {
				lines = append(lines, "warning: "+warning)
			}
			return emit(cmd, result, lines)
		}}
	cmd.Flags().StringVar(&dir, "dir", ".", "repository directory")
	cmd.Flags().IntVar(&opts.Limit, "limit", codeindex.DefaultLimit, "maximum returned breadcrumbs")
	cmd.Flags().BoolVar(&opts.UseReranker, "rerank", false, "opt in to local cross-encoder reranking (hybrid only; no floor)")
	cmd.Flags().BoolVar(&opts.NoRefresh, "no-refresh", false, "explicit stale-by-choice query of an existing index; safety checks still run")
	return cmd
}

func newEvalLocateCommand() *cobra.Command {
	var dir string
	var opts codeindex.EvalOptions
	var floor float32
	cmd := &cobra.Command{Use: "locate", Short: "Evaluate code Recall@1/@K and report nonsense probes against original golden JSON",
		Long: "Run production locate with lazy refresh for every query.\n" +
			"A supplied rerank floor filters only harness results; runtime locate has no floor.\n" +
			"Fusion's empty-probe leaks are reported. Missed match queries or failed requested\n" +
			"reranked safety probes exit nonzero. The bundled Rust golden needs its tagged reference\n" +
			"corpus; use --golden for your own repository (see tests/golden/testdata/locate/NOTICE.md).",
		Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			root, err := codeindex.ResolveRoot(dir)
			if err != nil {
				return err
			}
			file := opts.GoldenPath
			if !filepath.IsAbs(file) {
				file = filepath.Join(root, file)
			}
			golden, err := codeindex.LoadGolden(file)
			if err != nil {
				return err
			}
			opts.GoldenPath = file
			if cmd.Flags().Changed("min-rerank-score") {
				opts.MinRerankScore = &floor
			}
			summary, evalErr := codeindex.Evaluate(cmd.Context(), root, nil, golden, opts)
			if evalErr != nil && !errors.Is(evalErr, codeindex.ErrBelowBaseline) {
				return evalErr
			}
			lines := []string{fmt.Sprintf("locate Recall@1: %d/%d; Recall@%d: %d/%d (%.1f%%); empty probes: %d/%d (%d safety failures)",
				summary.MatchPassesAt1, summary.MatchQueries, summary.K, summary.MatchPassesAtK, summary.MatchQueries, summary.RecallAtK*100, summary.EmptyPasses, summary.EmptyQueries, summary.SafetyFailures)}
			for _, outcome := range summary.Outcomes {
				lines = append(lines, fmt.Sprintf("  %s (%s): passed=%t, returned=%d", outcome.ID, outcome.Kind, outcome.Passed, outcome.ReturnedCount))
				if outcome.FailureReason != nil {
					lines = append(lines, "    "+*outcome.FailureReason)
				}
			}
			return errors.Join(evalErr, emit(cmd, summary, lines))
		}}
	cmd.Flags().StringVar(&dir, "dir", ".", "repository directory")
	cmd.Flags().StringVar(&opts.GoldenPath, "golden", codeindex.DefaultGoldenPath, "golden JSON path (relative to repository)")
	cmd.Flags().IntVar(&opts.K, "k", 3, "top K results to evaluate")
	cmd.Flags().BoolVar(&opts.UseReranker, "rerank", false, "opt in to reranking for this evaluation")
	cmd.Flags().Float32Var(&floor, "min-rerank-score", 0, "harness-only rerank floor (ignored unless reranking actually runs)")
	return cmd
}
