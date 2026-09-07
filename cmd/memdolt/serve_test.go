package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kninetimmy/memdolt/internal/ipc"
	"github.com/kninetimmy/memdolt/internal/mcpserver"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

func TestServeCancellationClosesProtocolPendingWorkIPCAndStore(t *testing.T) {
	base := initStore(t)
	server := mcpserver.New("test")
	clientPipe, serverPipe := net.Pipe()
	clientSide := &mcp.IOTransport{Reader: clientPipe, Writer: clientPipe}
	serverSide := &mcp.IOTransport{Reader: struct {
		io.Reader
		io.Closer
	}{localdolt.JSONUnicodeReader(serverPipe), serverPipe}, Writer: stdioWriter{serverPipe}}
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
	// The output closer is a no-op, as for stdout. Only the original input
	// closer can close this pipe and unblock the client's pending read.
	if err := clientPipe.SetReadDeadline(time.Now().Add(time.Second)); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("guarded input was not closed on cancellation: %v", err)
	}
	assertServeReleased(t, base)
}

func TestServeGuardedStdioRetainsLegacyBatchNegotiation(t *testing.T) {
	for _, version := range []string{"2025-03-26", "2025-11-25"} {
		t.Run(version, func(t *testing.T) {
			base := initStore(t)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestServeHelperProcess$")
			cmd.Env = append(os.Environ(), "MEMDOLT_SERVE_HELPER=1", "MEMDOLT_SERVE_DIR="+base)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			input, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			output, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			encoder, decoder := json.NewEncoder(input), json.NewDecoder(output)
			if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": version, "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "legacy-batch-test", "version": "1"}}}); err != nil {
				t.Fatal(err)
			}
			var initialized struct {
				Result struct {
					ProtocolVersion string `json:"protocolVersion"`
				} `json:"result"`
			}
			if err := decoder.Decode(&initialized); err != nil || initialized.Result.ProtocolVersion != version {
				t.Fatalf("genuine legacy negotiation = %+v, %v", initialized, err)
			}
			if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}); err != nil {
				t.Fatal(err)
			}
			batch := []map[string]any{
				{"jsonrpc": "2.0", "id": 2, "method": "ping", "params": map[string]any{"_meta": map[string]string{"large": strings.Repeat("x", 70*1024) + "😀�"}}},
				{"jsonrpc": "2.0", "id": 3, "method": "ping"},
			}
			if err := encoder.Encode(batch); err != nil {
				t.Fatal(err)
			}
			var responses []struct {
				ID int `json:"id"`
			}
			decodeErr := decoder.Decode(&responses)
			_ = input.Close()
			waitErr := cmd.Wait()
			if version == "2025-03-26" {
				if decodeErr != nil || waitErr != nil || len(responses) != 2 || responses[0].ID != 2 || responses[1].ID != 3 {
					t.Fatalf("valid batch = %+v, %v, %v, stderr=%s", responses, decodeErr, waitErr, &stderr)
				}
			} else if decodeErr == nil || waitErr == nil || !strings.Contains(stderr.String(), "batching is not supported") {
				t.Fatalf("newer legacy batch restriction lost: %v, %v, stderr=%s", decodeErr, waitErr, &stderr)
			}
			assertServeReleased(t, base)
		})
	}
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
	if len(tools.Tools) != 22 {
		t.Fatalf("serve advertised %d tools, want the 21 existing tools including repo_pull and repo_push plus locate", len(tools.Tools))
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
