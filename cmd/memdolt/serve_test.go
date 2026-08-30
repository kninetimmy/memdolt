package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kninetimmy/memdolt/internal/ipc"
	"github.com/kninetimmy/memdolt/internal/mcpserver"
)

func TestServeCancellationClosesProtocolPendingWorkIPCAndStore(t *testing.T) {
	base := initStore(t)
	server := mcpserver.New("test")
	clientSide, serverSide := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pending := closeFunc{fn: func() error {
		for range server.Sessions() {
			return errors.New("pending work closed before protocol serving")
		}
		status, _, err := ipc.Probe(context.Background(), base)
		if err != nil {
			return err
		}
		if status != ipc.StatusOwnerLive {
			return fmt.Errorf("pending work saw IPC status %s, want live", status)
		}
		return nil
	}}

	done := make(chan error, 1)
	go func() {
		done <- runServe(ctx, base, server, serverSide, &pending)
	}()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), clientSide, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()

	status, _, err := ipc.Probe(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	if status != ipc.StatusOwnerLive {
		t.Fatalf("serve IPC status = %s, want live", status)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not stop after cancellation")
	}
	if !pending.closed {
		t.Fatal("pending work was not closed")
	}
	assertServeReleased(t, base)
}

func TestServeCommandUsesStdioWithoutNonProtocolOutput(t *testing.T) {
	base := initStore(t)
	var stderr bytes.Buffer
	cmd := exec.Command(os.Args[0], "-test.run=^TestServeHelperProcess$")
	cmd.Env = append(os.Environ(), "MEMDOLT_SERVE_HELPER=1", "MEMDOLT_SERVE_DIR="+base)
	cmd.Stderr = &stderr

	client := mcp.NewClient(&mcp.Implementation{Name: "stdio-test", Version: "1"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connect to serve command: %v (stderr %q)", err, stderr.String())
	}
	if session.InitializeResult().ProtocolVersion != "2026-07-28" {
		t.Fatalf("serve protocol = %q", session.InitializeResult().ProtocolVersion)
	}
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("tools/list: %v (stderr %q)", err, stderr.String())
	}
	if tools.TTLMs <= 0 {
		t.Fatalf("tools/list ttlMs = %d, want positive", tools.TTLMs)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("close serve command: %v (stderr %q)", err, stderr.String())
	}
	assertServeReleased(t, base)
}

func TestServeHelperProcess(t *testing.T) {
	if os.Getenv("MEMDOLT_SERVE_HELPER") != "1" {
		return
	}
	root := newRootCommand()
	root.SetArgs([]string{"serve", "--dir", os.Getenv("MEMDOLT_SERVE_DIR")})
	if err := root.ExecuteContext(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

type closeFunc struct {
	fn     func() error
	closed bool
}

func (c *closeFunc) Close() error {
	c.closed = true
	return c.fn()
}

func assertServeReleased(t *testing.T, base string) {
	t.Helper()
	status, _, err := ipc.Probe(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	if status != ipc.StatusNoOwner {
		t.Fatalf("IPC status after serve = %s, want no-owner", status)
	}
	st := openInitializedStore(t, base)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
}
