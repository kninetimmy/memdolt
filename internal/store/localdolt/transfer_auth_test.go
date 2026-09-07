package localdolt

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dolthub/dolt/go/libraries/doltcore/dbfactory"
	"github.com/dolthub/dolt/go/libraries/doltcore/env"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func TestTransferAuthenticationOverrideAnonymousRedactionAndCancellation(t *testing.T) {
	const password = "synthetic-transfer-password"
	t.Setenv("DOLT_REMOTE_PASSWORD", password)
	// A native username-less SQL transfer would attempt this personal credential
	// and fail before reaching the server. The private bridge must stay anonymous.
	home := t.TempDir()
	t.Setenv("DOLT_ROOT_PATH", home)
	if err := os.Mkdir(filepath.Join(home, ".dolt"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".dolt", "config_global.json"), []byte(`{"user.creds":"synthetic-missing-credential","metrics.disabled":"true"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	a, _ := transferFixture(t)
	for _, operation := range []string{"push", "pull"} {
		for _, selection := range []struct {
			stored, user string
			cancel       bool
		}{
			{"stored", "override", false}, {"stored", "", false}, {"", "", false}, {"", "cancel-user", true},
		} {
			t.Run(operation+"/"+selection.stored+"/"+selection.user, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				headers := make(chan string, 1)
				server := grpc.NewServer(grpc.UnknownServiceHandler(func(_ any, stream grpc.ServerStream) error {
					md, _ := metadata.FromIncomingContext(stream.Context())
					header := strings.Join(md.Get("authorization"), " ")
					select {
					case headers <- header:
					default:
					}
					if selection.cancel {
						cancel()
						return status.Error(codes.Canceled, "synthetic cancellation")
					}
					return status.Error(codes.Unauthenticated, "synthetic auth refusal "+password+" "+header)
				}))
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				done := make(chan error, 1)
				go func() { done <- server.Serve(listener) }()
				defer func() { server.Stop(); <-done }()
				params := "{}"
				if selection.stored != "" {
					params = `{"` + dbfactory.GRPCUsernameAuthParam + `":"` + selection.stored + `"}`
				}
				a = transferConfiguredRemote(t, a, "http://"+listener.Addr().String()+"/fixture", params)
				got, err := a.transfer(ctx, operation, TransferOptions{User: selection.user}, transferHooks{})
				if err == nil || strings.Contains(err.Error(), password) {
					t.Fatalf("transfer = %+v, %v", got, err)
				}
				user := selection.user
				if user == "" {
					user = selection.stored
				}
				want := ""
				if user != "" {
					want = "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+password))
				}
				if want != "" && strings.Contains(err.Error(), want) {
					t.Fatal("Basic credentials leaked")
				}
				select {
				case header := <-headers:
					if header != want {
						t.Fatal("wrong per-call auth header")
					}
				default:
					t.Fatal("transfer did not reach synthetic auth server")
				}
				config, err := os.ReadFile(filepath.Join(a.DataDir(), DatabaseName, ".dolt", "repo_state.json"))
				if err != nil || strings.Contains(string(config), password) || (want != "" && strings.Contains(string(config), want)) {
					t.Fatal("transfer persisted credentials")
				}
			})
		}
	}
}

func TestTransferInputAndPrivateCapabilityFailClosed(t *testing.T) {
	a, _ := transferFixture(t)
	ctx := context.Background()
	if _, err := a.db.Exec("CALL memdolt_transfer()"); err == nil || !strings.Contains(err.Error(), "capability") {
		t.Fatalf("ordinary SQL reached private bridge: %v", err)
	}
	for _, name := range []string{"--force", "-f", "origin:main", "https://host/db", "origin/main", " origin", "' OR 1=1"} {
		if _, err := a.Push(ctx, TransferOptions{Remote: name}); err == nil {
			t.Fatalf("accepted name %q", name)
		}
	}
	for _, raw := range []string{"http://user:synthetic@host/db", "https://host/db?password=synthetic", "https://host/db#synthetic", "ssh://host/db", "file:relative", "http://host:0/db", "http://host:65536/db"} {
		a = transferConfiguredRemote(t, a, raw, "{}")
		if _, err := a.Pull(ctx, TransferOptions{}); err == nil || strings.Contains(err.Error(), "synthetic") {
			t.Fatalf("URL was accepted or exposed: %v", err)
		}
	}
	for _, params := range []string{`{"journal":"true"}`, `{"password":"synthetic"}`, `{"__DOLT__grpc_username":"--force"}`} {
		a = transferConfiguredRemote(t, a, "http://127.0.0.1:1/never-contact", params)
		if _, err := a.Push(ctx, TransferOptions{User: "override"}); err == nil || strings.Contains(err.Error(), "synthetic") {
			t.Fatalf("params = %v", err)
		}
	}
	a = transferConfiguredRemote(t, a, "http://127.0.0.1:1/never-contact", "{}")
	t.Setenv("DOLT_REMOTE_PASSWORD", "synthetic")
	for _, user := range []string{"--force", "user:password", " user", "user\n", strings.Repeat("x", 33)} {
		if _, err := a.Push(ctx, TransferOptions{User: user}); err == nil {
			t.Fatalf("accepted user %q", user)
		}
	}
	if err := os.Unsetenv("DOLT_REMOTE_PASSWORD"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Push(ctx, TransferOptions{User: "valid"}); err == nil || !strings.Contains(err.Error(), "restart the owner") {
		t.Fatalf("missing password = %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := a.Pull(canceled, TransferOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled transfer = %v", err)
	}
}

// Model externally configured remotes while the owner is stopped, including
// malformed options the ordinary Dolt remote-add parser itself would refuse.
func transferConfiguredRemote(t *testing.T, s *Store, raw, params string) *Store {
	t.Helper()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.DataDir(), DatabaseName, ".dolt", "repo_state.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]json.RawMessage
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	var remotes map[string]env.Remote
	if err := json.Unmarshal(state["remotes"], &remotes); err != nil {
		t.Fatal(err)
	}
	remote := remotes["origin"]
	remote.Url = raw
	remote.Params = nil
	if err := json.Unmarshal([]byte(params), &remote.Params); err != nil {
		t.Fatal(err)
	}
	remotes["origin"] = remote
	state["remotes"], err = json.Marshal(remotes)
	if err != nil {
		t.Fatal(err)
	}
	data, err = json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	next, err := New(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := next.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = next.Close() })
	return next
}
