package main

import (
	"context"
	"errors"
	"io"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/kninetimmy/memdolt/internal/ipc"
	"github.com/kninetimmy/memdolt/internal/mcpserver"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
	"github.com/kninetimmy/memdolt/internal/storeipc"
)

func newServeCommand() *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Serve memdolt tools over MCP stdio",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runServe(cmd.Context(), dir, mcpserver.New(resolveVersion().Version), &mcp.StdioTransport{}, nil)
		},
	}
	cmd.Flags().StringVar(&dir, "dir", ".",
		"repository root whose store to serve (the store lives in <dir>/.memdolt)")
	return cmd
}

// runServe owns shutdown ordering for one stdio session. Protocol serving ends
// first; pending work can still use the live endpoint and store; then IPC and
// the embedded store close in that order.
func runServe(
	ctx context.Context,
	baseDir string,
	server *mcp.Server,
	transport mcp.Transport,
	pendingWork io.Closer,
) (err error) {
	st, err := localdolt.New(localdolt.Config{BaseDir: baseDir, Actor: cliActor})
	if err != nil {
		return err
	}
	if err := st.Open(ctx); err != nil {
		return err
	}
	defer func() { err = errors.Join(err, st.Close()) }()

	owner := &localCommandStore{Store: st, baseDir: baseDir}
	if err := requireCurrentSchema(ctx, owner); err != nil {
		return err
	}
	routes, err := storeipc.NewHandler(storeipc.Config{
		Store:        owner,
		ReviewAccept: owner.ReviewAcceptExpected,
	})
	if err != nil {
		return err
	}
	endpoint, err := ipc.Listen(ipc.Config{BaseDir: baseDir, Handler: routes})
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, endpoint.Close()) }()
	tools := mcpserver.RegisterTools(server, baseDir, owner)
	defer func() { err = errors.Join(err, tools.Close()) }()
	if pendingWork != nil {
		defer func() { err = errors.Join(err, pendingWork.Close()) }()
	}

	err = server.Run(ctx, transport)
	if err == context.Canceled && ctx.Err() == context.Canceled {
		err = nil
	}
	return err
}
