package storeipc

import (
	"context"
	"errors"
	"fmt"

	"github.com/kninetimmy/memdolt/internal/ipc"
)

// Client submits store operations to the process that owns a repository's
// store.
//
// It adds nothing to the transport underneath it, and in particular no
// retry: a failed operation is reported to the caller, never resubmitted
// (docs/spikes/m0-rig1.md, F3 — a write over IPC is at-least-once). The
// connection itself is re-established by internal/ipc, so a client keeps
// working across an owner restart without being rebuilt.
type Client struct {
	ipc *ipc.Client
}

// Dial finds the live owner of baseDir and returns a client for it. It
// fails if no live process owns the repository — the caller is then the
// one that may open the store directly (PRD §5.2.1).
func Dial(baseDir string) (*Client, error) {
	conn, err := ipc.Dial(baseDir)
	if err != nil {
		return nil, err
	}
	return NewClient(conn), nil
}

// NewClient wraps an already-dialled endpoint client.
func NewClient(conn *ipc.Client) *Client {
	return &Client{ipc: conn}
}

// Commit asks the owner to apply a write and record it as one Dolt commit.
//
// An error that matches *ipc.StatusError means the owner received the
// request and the write did not happen. Any other error means the outcome
// is unknown: the owner may have committed and the answer been lost. A
// caller that must not report a write it cannot vouch for has to keep
// those apart. IsOwnerRefusal is that test.
//
// An error of the second kind that also matches ipc.ErrNoLiveOwner says
// something further: no live owner holds the store any more, so the caller
// may open it directly rather than dial again. The write's own outcome is
// still unknown — that is what makes it a separate question from the
// refusal test above.
func (c *Client) Commit(ctx context.Context, req CommitRequest) (CommitResponse, error) {
	var resp CommitResponse
	if err := c.ipc.PostJSON(ctx, CommitPath, req, &resp); err != nil {
		return CommitResponse{}, fmt.Errorf("storeipc: commit: %w", err)
	}
	return resp, nil
}

// Query asks the owner to run exactly one read-only SELECT or SHOW statement;
// the owner rejects every other form before execution. Its errors classify
// exactly as Commit's do, less the question of whether a write happened.
func (c *Client) Query(ctx context.Context, req QueryRequest) (QueryResponse, error) {
	var resp QueryResponse
	if err := c.ipc.PostJSON(ctx, QueryPath, req, &resp); err != nil {
		return QueryResponse{}, fmt.Errorf("storeipc: query: %w", err)
	}
	return resp, nil
}

// IsOwnerRefusal reports whether err is the owner refusing or failing a
// request, as opposed to the request never reaching a verdict.
func IsOwnerRefusal(err error) bool {
	var status *ipc.StatusError
	return errors.As(err, &status)
}
