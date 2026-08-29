package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/kninetimmy/memdolt/internal/embedding"
	"github.com/kninetimmy/memdolt/internal/layout"
	"github.com/kninetimmy/memdolt/internal/memory"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

func newIndexCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "index",
		Short: "Inspect or rebuild the derived embedding index",
		Long: "The embedding index is derived, machine-local SQLite data (PRD §8.2).\n" +
			"Its vectors never enter Dolt history and can always be rebuilt from durable text.",
	}
	cmd.AddCommand(newIndexRebuildCommand(), newIndexStatusCommand())
	return cmd
}

func newIndexRebuildCommand() *cobra.Command {
	var flags storeFlags
	cmd := &cobra.Command{
		Use:   "rebuild",
		Short: "Synchronize the embedding side-store with durable memory",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return flags.runStore(cmd, func(ctx context.Context, st *localdolt.Store, _ memory.Actor) error {
				sources, err := st.EmbeddingSources(ctx)
				if err != nil {
					return err
				}
				paths, err := layout.New(flags.dir)
				if err != nil {
					return err
				}

				// An unchanged rebuild never calls this closure, so it neither loads
				// ONNX nor fetches already-unneeded artifacts.
				var engine *embedding.Engine
				result, rebuildErr := embedding.Rebuild(ctx, paths.EmbeddingsFile(), sources, func(text string) ([]float32, error) {
					if engine == nil {
						engine, err = embedding.Open(ctx, embedding.Options{})
						if err != nil {
							return nil, err
						}
					}
					return engine.Embed(text)
				})
				if engine != nil {
					rebuildErr = errors.Join(rebuildErr, engine.Close())
				}
				if rebuildErr != nil {
					return rebuildErr
				}
				return emit(cmd, result, []string{fmt.Sprintf(
					"embedding index at %s is current: %d created, %d refreshed, %d unchanged, %d orphaned removed",
					result.Path, result.Created, result.Refreshed, result.Unchanged, result.Removed)})
			})
		},
	}
	return flags.bind(cmd)
}

func newIndexStatusCommand() *cobra.Command {
	var flags storeFlags
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Compare derived embeddings with durable source text",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return flags.runStore(cmd, func(ctx context.Context, st *localdolt.Store, _ memory.Actor) error {
				sources, err := st.EmbeddingSources(ctx)
				if err != nil {
					return err
				}
				paths, err := layout.New(flags.dir)
				if err != nil {
					return err
				}
				report, err := embedding.Status(ctx, paths.EmbeddingsFile(), sources)
				if err != nil {
					return err
				}
				lines := []string{fmt.Sprintf(
					"embedding index at %s: %d current, %d missing, %d content-hash-mismatched, %d wrong-byte-length, %d orphaned",
					report.Path, report.Current, report.Missing, report.ContentHashMismatched, report.WrongByteLength, report.Orphaned)}
				for _, entry := range report.Entries {
					if entry.State != embedding.StatusCurrent {
						lines = append(lines, fmt.Sprintf("  %s/%s (%s): %s", entry.SourceType, entry.SourceID, entry.ModelName, entry.State))
					}
				}
				if report.NeedsRebuild {
					lines = append(lines, "remedy: "+report.Remedy)
				} else {
					lines = append(lines, "embedding index is current")
				}
				return emit(cmd, report, lines)
			})
		},
	}
	return flags.bind(cmd)
}
