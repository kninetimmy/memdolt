package main

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/kninetimmy/memdolt/internal/memory"
	"github.com/kninetimmy/memdolt/internal/search"
)

func newSearchCommand() *cobra.Command {
	var flags storeFlags
	var limit int
	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Search committed decisions or cached Git file history",
		Long: "Use file:<repository-relative-path> for cached Git history, or an indexed path.\n" +
			"File queries never open Dolt, read source bodies, run Git or provision models.\n" +
			"Run ingest-git explicitly to populate history; results may cover only selected ranges.\n" +
			"Explicit decision prefixes and other queries search committed decision titles and rationales.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			query, err := search.Parse(args[0], limit)
			if err != nil {
				return err
			}
			if response, handled, err := search.TryFile(cmd.Context(), flags.dir, query); handled {
				if err != nil {
					return err
				}
				return emit(cmd, response, search.Lines(response))
			}
			return flags.runStore(cmd, func(ctx context.Context, st commandStore, _ memory.Actor) error {
				response, err := search.Run(ctx, st, query)
				if err != nil {
					return err
				}
				return emit(cmd, response, search.Lines(response))
			})
		},
	}
	cmd.Flags().IntVar(&limit, "limit", search.DefaultLimit, "maximum matches (cached file history capped at 200000)")
	return flags.bind(cmd)
}
