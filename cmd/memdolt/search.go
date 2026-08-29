package main

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/kninetimmy/memdolt/internal/memory"
	"github.com/kninetimmy/memdolt/internal/search"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

func newSearchCommand() *cobra.Command {
	var flags storeFlags
	var limit int
	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Search committed decision titles and rationales",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			query, err := search.Parse(args[0], limit)
			if err != nil {
				return err
			}
			return flags.runStore(cmd, func(ctx context.Context, st *localdolt.Store, _ memory.Actor) error {
				response, err := search.Run(ctx, st, query)
				if err != nil {
					return err
				}
				return emit(cmd, response, search.Lines(response))
			})
		},
	}
	cmd.Flags().IntVar(&limit, "limit", search.DefaultLimit, "maximum decision matches to return")
	return flags.bind(cmd)
}
