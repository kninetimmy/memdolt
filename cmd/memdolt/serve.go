package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

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
		Long: "Serve the configured local/clone repository over MCP stdio. live topology\n" +
			"is unsupported. Opt-in [repo] auto_pull_on_session_start runs existing pull\n" +
			"once before publishing this owner; unresolved conflicts or failures stop\n" +
			"startup with an inspection remedy. No automatic retries or approvals occur.\n" +
			"Protocol stdout carries MCP only. Restart the owner after changing topology.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			transport := &mcp.IOTransport{
				Reader: struct {
					io.Reader
					io.Closer
				}{localdolt.JSONUnicodeReader(os.Stdin), os.Stdin},
				Writer: stdioWriter{os.Stdout},
			}
			return runServe(cmd.Context(), dir, mcpserver.New(resolveVersion().Version), transport, nil)
		},
	}
	cmd.Flags().StringVar(&dir, "dir", ".",
		"repository root whose store to serve (the store lives in <dir>/.memdolt)")
	return cmd
}

// Match StdioTransport's stdout lifetime while guarding its input bytes. The
// SDK's actual IO connection still owns protocol negotiation and batch rules.
type stdioWriter struct{ io.Writer }

func (stdioWriter) Close() error { return nil }

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
	var startup localdolt.TransferResult
	defer func() {
		err = errors.Join(err, st.Close())
		if err != nil && startup.Changed {
			err = fmt.Errorf("session-start pull confirmed main %s; inspect history before retrying startup: %w", startup.MainCommit, err)
		}
	}()

	owner := &localCommandStore{Store: st, baseDir: baseDir}
	if err := requireCurrentSchema(ctx, owner); err != nil {
		return err
	}
	startup, err = st.PullOnSessionStart(ctx)
	if err != nil {
		return err
	}
	tools := mcpserver.RegisterTools(server, baseDir, owner)
	// Install before publishing IPC so every reached render uses this queue.
	// Close still runs before endpoint/store shutdown, including setup failure.
	var endpoint *ipc.Server
	defer func() {
		err = errors.Join(err, tools.Close())
		if endpoint != nil {
			err = errors.Join(err, endpoint.Close())
		}
	}()
	routes, err := storeipc.NewHandler(storeipc.Config{
		Store:        owner,
		ReviewAccept: owner.ReviewAcceptExpected,
		Render:       tools.Render,
	})
	if err != nil {
		return err
	}
	endpoint, err = ipc.Listen(ipc.Config{BaseDir: baseDir, Handler: routes})
	if err != nil {
		return err
	}
	if pendingWork != nil {
		defer func() { err = errors.Join(err, pendingWork.Close()) }()
	}

	err = server.Run(ctx, transport)
	if err == context.Canceled && ctx.Err() == context.Canceled {
		err = nil
	}
	return err
}
