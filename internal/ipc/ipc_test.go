package ipc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kninetimmy/memdolt/internal/layout"
	"github.com/kninetimmy/memdolt/internal/singleowner"
)

// helperBaseDirEnv puts the test binary into helper mode: it starts an
// endpoint for the named repository and holds it until killed. Probing a
// pidfile whose owner was killed is only meaningful against a real second
// process, because what makes the pidfile detectably orphaned is the
// kernel releasing the lock the dead process held.
const helperBaseDirEnv = "MEMDOLT_TEST_IPC_HELPER_BASE"

func TestMain(m *testing.M) {
	if base := os.Getenv(helperBaseDirEnv); base != "" {
		runServerHelper(base)
		return
	}
	os.Exit(m.Run())
}

func runServerHelper(base string) {
	srv, err := Listen(Config{BaseDir: base, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		fmt.Fprintln(os.Stderr, "ipc helper:", err)
		os.Exit(2)
	}
	fmt.Println("READY", os.Getpid(), srv.Port())
	_, _ = io.Copy(io.Discard, os.Stdin)
	if err := srv.Close(); err != nil {
		fmt.Fprintln(os.Stderr, "ipc helper close:", err)
		os.Exit(3)
	}
}

type helper struct {
	cmd   *exec.Cmd
	pid   int
	port  int
	stdin io.WriteCloser
	done  bool
}

func startServerHelper(t *testing.T, base string) *helper {
	t.Helper()

	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), helperBaseDirEnv+"="+base)
	cmd.Stderr = os.Stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("helper stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("helper stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start ipc helper: %v", err)
	}

	h := &helper{cmd: cmd, stdin: stdin}
	t.Cleanup(func() {
		if h.done {
			return
		}
		_ = h.stdin.Close()
		_ = h.cmd.Process.Kill()
		_ = h.cmd.Wait()
	})

	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("read helper readiness (got %q): %v", line, err)
	}
	fields := strings.Fields(line)
	if len(fields) != 3 || fields[0] != "READY" {
		t.Fatalf("unexpected helper greeting %q", line)
	}
	if h.pid, err = strconv.Atoi(fields[1]); err != nil {
		t.Fatalf("parse helper pid from %q: %v", line, err)
	}
	if h.port, err = strconv.Atoi(fields[2]); err != nil {
		t.Fatalf("parse helper port from %q: %v", line, err)
	}
	return h
}

func (h *helper) kill(t *testing.T) {
	t.Helper()
	if err := h.cmd.Process.Kill(); err != nil {
		t.Fatalf("kill helper: %v", err)
	}
	// Wait reaps the process; by the time it returns the kernel has
	// released every lock the child held.
	_ = h.cmd.Wait()
	h.done = true
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func startServer(t *testing.T, cfg Config) (*Server, *syncBuffer) {
	t.Helper()
	logs := &syncBuffer{}
	cfg.Logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	srv, err := Listen(cfg)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv, logs
}

func TestListenBindsLoopbackOnAnEphemeralPort(t *testing.T) {
	srv, _ := startServer(t, Config{BaseDir: t.TempDir()})

	if !strings.HasPrefix(srv.Addr(), "127.0.0.1:") {
		t.Fatalf("endpoint address = %q, want a 127.0.0.1 address", srv.Addr())
	}
	if srv.Port() <= 0 {
		t.Fatalf("endpoint port = %d, want an ephemeral port", srv.Port())
	}
}

func TestPidFileRecordsOwnerPortAndToken(t *testing.T) {
	base := t.TempDir()
	srv, _ := startServer(t, Config{BaseDir: base})

	paths, err := layout.New(base)
	if err != nil {
		t.Fatalf("layout: %v", err)
	}
	if srv.PidFile() != paths.PidFile() {
		t.Fatalf("pidfile = %q, want %q", srv.PidFile(), paths.PidFile())
	}
	if filepath.Dir(srv.PidFile()) != paths.Dir() {
		t.Fatalf("pidfile %q is not beneath %q", srv.PidFile(), paths.Dir())
	}

	info, err := os.Stat(srv.PidFile())
	if err != nil {
		t.Fatalf("stat pidfile: %v", err)
	}
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("pidfile mode = %v, want 0600: it holds a live credential", perm)
		}
	}

	raw, err := os.ReadFile(srv.PidFile())
	if err != nil {
		t.Fatalf("read pidfile: %v", err)
	}
	var record struct {
		PID    int `json:"pid"`
		Detail struct {
			Port  int    `json:"port"`
			Token string `json:"token"`
		} `json:"detail"`
	}
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatalf("decode pidfile %s: %v", raw, err)
	}
	if record.PID != os.Getpid() {
		t.Fatalf("pidfile pid = %d, want %d", record.PID, os.Getpid())
	}
	if record.Detail.Port != srv.Port() {
		t.Fatalf("pidfile port = %d, want %d", record.Detail.Port, srv.Port())
	}
	if record.Detail.Token != string(srv.token) {
		t.Fatalf("pidfile does not carry the endpoint's token")
	}
	if len(record.Detail.Token) != tokenBytes*2 {
		t.Fatalf("token is %d hex characters, want %d", len(record.Detail.Token), tokenBytes*2)
	}
}

// TestEndpointRejectsRequestsWithoutTheToken covers the acceptance
// criterion that the listener rejects any request whose token does not
// match the pidfile's.
func TestEndpointRejectsRequestsWithoutTheToken(t *testing.T) {
	base := t.TempDir()
	gated := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	srv, _ := startServer(t, Config{BaseDir: base, Handler: gated})

	valid := string(srv.token)
	wrong := strings.Repeat("f", len(valid))
	if wrong == valid {
		t.Fatal("the wrong token must differ from the real one")
	}

	cases := []struct {
		name  string
		path  string
		token string
		want  int
	}{
		{"health without a token", HealthPath, "", http.StatusUnauthorized},
		{"health with a wrong token", HealthPath, wrong, http.StatusUnauthorized},
		{"health with a truncated token", HealthPath, valid[:len(valid)-1], http.StatusUnauthorized},
		{"health with the token", HealthPath, valid, http.StatusOK},
		{"owner route without a token", "/anything", "", http.StatusUnauthorized},
		{"owner route with a wrong token", "/anything", wrong, http.StatusUnauthorized},
		{"owner route with the token", "/anything", valid, http.StatusTeapot},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(context.Background(),
				http.MethodGet, "http://"+srv.Addr()+tc.path, nil)
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			if tc.token != "" {
				req.Header.Set(TokenHeader, tc.token)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			if strings.Contains(string(body), valid) {
				t.Fatal("response body echoed the token")
			}
		})
	}
}

// TestTokenNeverReachesLogsOrErrors covers the acceptance criterion that no
// token value is ever written to a log or an error message.
func TestTokenNeverReachesLogsOrErrors(t *testing.T) {
	base := t.TempDir()
	srv, logs := startServer(t, Config{BaseDir: base})
	token := string(srv.token)

	// Formatting the secret type must not reveal it, whichever verb is used.
	for _, verb := range []string{"%v", "%s", "%q", "%#v"} {
		if formatted := fmt.Sprintf(verb, srv.token); strings.Contains(formatted, token) {
			t.Fatalf("formatting a token with %s revealed it: %s", verb, formatted)
		}
	}
	// Including when it is reached through the struct it is stored in.
	detail := ownerDetail{Port: srv.Port(), Token: srv.token}
	for _, verb := range []string{"%v", "%+v", "%#v"} {
		if formatted := fmt.Sprintf(verb, detail); strings.Contains(formatted, token) {
			t.Fatalf("formatting the pidfile detail with %s revealed the token: %s", verb, formatted)
		}
	}

	// Exercise the paths that could plausibly log or wrap it.
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		"http://"+srv.Addr()+HealthPath, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set(TokenHeader, "not-the-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_ = resp.Body.Close()

	status, _, err := Probe(context.Background(), base)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if status != StatusOwnerLive {
		t.Fatalf("probe status = %v, want %v", status, StatusOwnerLive)
	}

	// A probe against a corrupt record must not quote the record back.
	corrupt := filepath.Join(t.TempDir(), "corrupt")
	if err := os.MkdirAll(filepath.Join(corrupt, layout.DirName), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	corruptPaths, err := layout.New(corrupt)
	if err != nil {
		t.Fatalf("layout: %v", err)
	}
	record := fmt.Sprintf(`{"pid":%d,"host":%q,"id":"x","acquiredAt":"2026-07-29T00:00:00Z","detail":{"port":0,"token":%q}}`,
		os.Getpid(), hostname(t), token)
	if err := os.WriteFile(corruptPaths.PidFile(), []byte(record), 0o600); err != nil {
		t.Fatalf("write record: %v", err)
	}
	_, _, probeErr := Probe(context.Background(), corrupt)
	if probeErr != nil && strings.Contains(probeErr.Error(), token) {
		t.Fatalf("probe error leaked the token: %v", probeErr)
	}

	if logged := logs.String(); strings.Contains(logged, token) {
		t.Fatalf("the token reached the log: %s", logged)
	}
}

func hostname(t *testing.T) string {
	t.Helper()
	name, err := os.Hostname()
	if err != nil {
		t.Fatalf("hostname: %v", err)
	}
	return name
}

// TestProbeDistinguishesLiveDeadAndAbsentOwners covers the acceptance
// criterion that the liveness probe returns three distinguishable
// outcomes.
func TestProbeDistinguishesLiveDeadAndAbsentOwners(t *testing.T) {
	t.Run("no owner", func(t *testing.T) {
		base := t.TempDir()
		status, info, err := Probe(context.Background(), base)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if status != StatusNoOwner {
			t.Fatalf("status = %v, want %v", status, StatusNoOwner)
		}
		if info.PID != 0 {
			t.Fatalf("info = %+v, want the zero value", info)
		}
	})

	t.Run("live owner", func(t *testing.T) {
		base := t.TempDir()
		srv, _ := startServer(t, Config{BaseDir: base})

		status, info, err := Probe(context.Background(), base)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if status != StatusOwnerLive {
			t.Fatalf("status = %v, want %v", status, StatusOwnerLive)
		}
		if info.PID != os.Getpid() || info.Port != srv.Port() {
			t.Fatalf("info = %+v, want pid %d port %d", info, os.Getpid(), srv.Port())
		}
		if info.OwnerID != srv.OwnerID() {
			t.Fatalf("info owner id = %q, want %q", info.OwnerID, srv.OwnerID())
		}
	})

	t.Run("dead owner", func(t *testing.T) {
		base := t.TempDir()
		h := startServerHelper(t, base)

		status, _, err := Probe(context.Background(), base)
		if err != nil {
			t.Fatalf("probe while the helper is running: %v", err)
		}
		if status != StatusOwnerLive {
			t.Fatalf("status while the helper runs = %v, want %v", status, StatusOwnerLive)
		}

		h.kill(t)

		status, info, err := Probe(context.Background(), base)
		if err != nil {
			t.Fatalf("probe after the helper was killed: %v", err)
		}
		if status != StatusOwnerDead {
			t.Fatalf("status after an unclean kill = %v, want %v", status, StatusOwnerDead)
		}
		if info.PID != h.pid || info.Port != h.port {
			t.Fatalf("info = %+v, want pid %d port %d", info, h.pid, h.port)
		}
	})

	t.Run("no owner after a clean shutdown", func(t *testing.T) {
		base := t.TempDir()
		srv, err := Listen(Config{BaseDir: base, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		if err := srv.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		if err := srv.Close(); err != nil {
			t.Fatalf("second close should be a no-op: %v", err)
		}

		status, _, err := Probe(context.Background(), base)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if status != StatusNoOwner {
			t.Fatalf("status = %v, want %v", status, StatusNoOwner)
		}
	})
}

func TestDialReachesTheLiveOwner(t *testing.T) {
	base := t.TempDir()
	srv, _ := startServer(t, Config{BaseDir: base})

	client, err := Dial(base)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	health, err := client.Health(context.Background())
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if !health.OK || health.PID != os.Getpid() || health.OwnerID != srv.OwnerID() {
		t.Fatalf("health = %+v, want ok for pid %d owner %s", health, os.Getpid(), srv.OwnerID())
	}

	if _, err := Dial(t.TempDir()); err == nil {
		t.Fatal("dialling a repository with no owner should fail")
	}
}

// servedBy is what the reconnection tests exchange over the endpoint: each
// owner answers with the name the test gave it, so a request that reached
// the wrong one is visible rather than merely successful.
type servedBy struct {
	Owner string `json:"owner"`
}

// namedOwner serves one route that reports the owner's test name and counts
// the requests that reached it.
func namedOwner(name string, requests *atomic.Int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(servedBy{Owner: name})
	})
}

// TestPostJSONReportsNoLiveOwnerWhenTheOwnerIsGone covers the acceptance
// criterion that a transport failure against a repository whose pidfile
// shows no live owner is reported as a distinct, programmatically
// recognisable outcome: the store is unheld, and the caller may open it
// directly.
func TestPostJSONReportsNoLiveOwnerWhenTheOwnerIsGone(t *testing.T) {
	// The two ways an owner stops being live are separate pidfile states,
	// and only one of them can be produced in-process: a killed owner
	// leaves its record behind for the kernel to orphan, which needs a real
	// second process.
	t.Run("owner killed", func(t *testing.T) {
		base := t.TempDir()
		h := startServerHelper(t, base)

		client, err := Dial(base)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		h.kill(t)

		err = client.PostJSON(context.Background(), "/anything", struct{}{}, nil)
		if !errors.Is(err, ErrNoLiveOwner) {
			t.Fatalf("error = %v, want one matching ErrNoLiveOwner", err)
		}
		// The status the pidfile is in is part of the diagnosis.
		if !strings.Contains(err.Error(), StatusOwnerDead.String()) {
			t.Fatalf("error %v does not report the pidfile as %v", err, StatusOwnerDead)
		}
	})

	t.Run("owner shut down cleanly", func(t *testing.T) {
		base := t.TempDir()
		srv, _ := startServer(t, Config{BaseDir: base})

		client, err := Dial(base)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		if err := srv.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}

		err = client.PostJSON(context.Background(), "/anything", struct{}{}, nil)
		if !errors.Is(err, ErrNoLiveOwner) {
			t.Fatalf("error = %v, want one matching ErrNoLiveOwner", err)
		}
		// A refusal is a different thing entirely, and must not be
		// confused with this one.
		var status *StatusError
		if errors.As(err, &status) {
			t.Fatalf("a transport failure was reported as an owner refusal: %v", err)
		}
	})
}

// TestPostJSONRecoversAcrossAnOwnerRestart covers the acceptance criterion
// that a client whose owner was replaced reaches the replacement — on its
// new port and with its new token — on the caller's next operation, without
// the caller rebuilding anything.
func TestPostJSONRecoversAcrossAnOwnerRestart(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()

	var firstRequests, secondRequests atomic.Int64
	first, _ := startServer(t, Config{BaseDir: base, Handler: namedOwner("first", &firstRequests)})

	client, err := Dial(base)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	var served servedBy
	if err := client.PostJSON(ctx, "/work", struct{}{}, &served); err != nil {
		t.Fatalf("operation against the first owner: %v", err)
	}
	if served.Owner != "first" {
		t.Fatalf("served by %q, want the first owner", served.Owner)
	}

	if err := first.Close(); err != nil {
		t.Fatalf("close the first owner: %v", err)
	}
	second, _ := startServer(t, Config{BaseDir: base, Handler: namedOwner("second", &secondRequests)})
	if second.Port() == first.Port() {
		t.Fatalf("the replacement owner was given the same port %d, so this run cannot show the client following a port change", second.Port())
	}
	if second.token == first.token {
		t.Fatal("the replacement owner reused the first owner's token")
	}

	// The operation that hits the dead endpoint fails: its outcome is
	// unknown, and nothing resubmits it.
	err = client.PostJSON(ctx, "/work", struct{}{}, &served)
	if err == nil {
		t.Fatal("an operation sent to the endpoint of a stopped owner reported success")
	}
	if errors.Is(err, ErrNoLiveOwner) {
		t.Fatalf("a live replacement owner was reported as no live owner: %v", err)
	}

	// The next one reaches the replacement, on the same client.
	served = servedBy{}
	if err := client.PostJSON(ctx, "/work", struct{}{}, &served); err != nil {
		t.Fatalf("operation after the restart: %v", err)
	}
	if served.Owner != "second" {
		t.Fatalf("served by %q, want the replacement owner", served.Owner)
	}
	if got := secondRequests.Load(); got != 1 {
		t.Fatalf("the replacement owner served %d requests, want exactly the one operation submitted to it", got)
	}
	if got := firstRequests.Load(); got != 1 {
		t.Fatalf("the first owner served %d requests, want exactly the one operation submitted to it", got)
	}
}

// TestReconnectionIsBoundedAndPaced covers the acceptance criterion that
// the probes a transport failure causes are counted and spaced: rig 1's
// clients redialled a dead owner without either, and produced 474,234
// connection-refused failures in about a minute (docs/spikes/m0-rig1.md,
// F4).
func TestReconnectionIsBoundedAndPaced(t *testing.T) {
	// The schedule itself, independently of any clock: strictly increasing
	// until it reaches the ceiling, and never above it.
	t.Run("schedule", func(t *testing.T) {
		p := pacing{attempts: 6, base: 100 * time.Millisecond, max: 400 * time.Millisecond}
		want := []time.Duration{0, 100, 200, 400, 400, 400}
		for i, ms := range want {
			attempt := i + 1
			if got := p.delay(attempt); got != ms*time.Millisecond {
				t.Fatalf("delay before attempt %d = %v, want %v", attempt, got, ms*time.Millisecond)
			}
		}
		if p.delay(64) != p.max {
			t.Fatalf("delay before a far attempt = %v, want the ceiling %v", p.delay(64), p.max)
		}

		// And the schedule a real client is built with: five probes, 1.1 s
		// of delay between them. That is the delay schedule alone — what a
		// caller waits is this plus each probe's own duration, which
		// reconnectBaseDelay documents.
		client := newClient(t.TempDir(), ownerDetail{Port: 1})
		if client.pacing.attempts != reconnectAttempts {
			t.Fatalf("a client allows %d probes per transport failure, want %d",
				client.pacing.attempts, reconnectAttempts)
		}
		var total time.Duration
		for attempt := 1; attempt <= client.pacing.attempts; attempt++ {
			total += client.pacing.delay(attempt)
		}
		if want := 1100 * time.Millisecond; total != want {
			t.Fatalf("the delays scheduled across %d probes total %v, want %v",
				client.pacing.attempts, total, want)
		}
	})

	t.Run("probes", func(t *testing.T) {
		base := t.TempDir()
		client := newClient(base, ownerDetail{Port: closedPort(t), Token: "unused"})
		client.pacing = pacing{attempts: 4, base: 25 * time.Millisecond, max: 200 * time.Millisecond}

		// An owner that holds the pidfile but does not answer is what the
		// repeated attempts exist for: Probe fails closed there rather than
		// returning a verdict, so the client keeps asking, bounded.
		var at []time.Time
		client.probe = func(context.Context, string) (Status, OwnerInfo, ownerDetail, error) {
			at = append(at, time.Now())
			return StatusNoOwner, OwnerInfo{}, ownerDetail{}, ErrOwnerUnverified
		}

		start := time.Now()
		err := client.PostJSON(context.Background(), "/work", struct{}{}, nil)
		if err == nil {
			t.Fatal("an operation against a closed port reported success")
		}
		if errors.Is(err, ErrNoLiveOwner) {
			t.Fatalf("an unverified owner was reported as no live owner: %v", err)
		}
		if !errors.Is(err, ErrOwnerUnverified) {
			t.Fatalf("error = %v, want it to carry why the probes failed", err)
		}

		if len(at) != client.pacing.attempts {
			t.Fatalf("one transport failure caused %d probes, want the bound of %d", len(at), client.pacing.attempts)
		}
		// Each probe waited at least its scheduled delay, and those delays
		// grow: the observed gaps are therefore increasing too.
		previous := start
		for i, when := range at {
			attempt := i + 1
			gap := when.Sub(previous)
			if want := client.pacing.delay(attempt); gap < want {
				t.Fatalf("probe %d came %v after the one before it, want at least %v", attempt, gap, want)
			}
			if attempt > 1 && client.pacing.delay(attempt) <= client.pacing.delay(attempt-1) {
				t.Fatalf("probe %d was not scheduled later than probe %d", attempt, attempt-1)
			}
			previous = when
		}
	})
}

// closedPort returns a loopback port with nothing listening on it, so that
// a request to it fails in the transport rather than reaching anything.
func closedPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", loopbackAddr)
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	port, err := listenerPort(listener)
	if err != nil {
		t.Fatalf("port: %v", err)
	}
	if err := listener.Close(); err != nil {
		t.Fatalf("close the reserved listener: %v", err)
	}
	return port
}

// TestReconnectionQueueEndsOnTheCallersContext covers the acceptance
// criterion that a caller whose context expires while its re-probe is
// queued behind another caller's round returns on its own deadline. Waiting
// for that turn was a sync.Mutex acquisition, which cannot be cancelled, so
// the wait was bounded by the other callers' probe schedules instead: four
// concurrent callers against an owner that accepts and never answers were
// measured returning after 64.4 s, whatever deadline each of them carried.
func TestReconnectionQueueEndsOnTheCallersContext(t *testing.T) {
	base := t.TempDir()
	client := newClient(base, ownerDetail{Port: closedPort(t), Token: "unused"})
	client.pacing = pacing{attempts: 2, base: time.Millisecond, max: time.Millisecond}

	// The first caller's round holds the serialization inside its first
	// probe until the test lets it go. A second caller that returns before
	// then returned without its turn, which is the whole point.
	probing := make(chan struct{})
	release := make(chan struct{})
	releaseHolder := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseHolder)

	var probes atomic.Int64
	client.probe = func(context.Context, string) (Status, OwnerInfo, ownerDetail, error) {
		if probes.Add(1) == 1 {
			close(probing)
			<-release
		}
		return StatusNoOwner, OwnerInfo{}, ownerDetail{}, ErrOwnerUnverified
	}

	holder := make(chan error, 1)
	go func() { holder <- client.PostJSON(context.Background(), "/work", struct{}{}, nil) }()
	<-probing

	deadline := 200 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()

	queued := make(chan error, 1)
	start := time.Now()
	go func() { queued <- client.PostJSON(ctx, "/work", struct{}{}, nil) }()

	var err error
	select {
	case err = <-queued:
	case <-time.After(50 * deadline):
		t.Fatalf("a caller with a %v deadline was still queued for its re-probe %v later", deadline, 50*deadline)
	}
	waited := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error after %v = %v, want one matching context.DeadlineExceeded", waited, err)
	}
	if got := probes.Load(); got != 1 {
		t.Fatalf("%d probes were made, want only the held round's: a caller that gave up must not have probed", got)
	}

	// The round that held the queue is undisturbed, and hands the
	// serialization on when it ends.
	releaseHolder()
	if err := <-holder; !errors.Is(err, ErrOwnerUnverified) {
		t.Fatalf("the holding caller's round ended with %v, want its own bounded failure", err)
	}
	if got := probes.Load(); got != int64(client.pacing.attempts) {
		t.Fatalf("the holding caller made %d probes, want the bound of %d", got, client.pacing.attempts)
	}
}

func TestListenRefusesASecondOwner(t *testing.T) {
	base := t.TempDir()
	first, _ := startServer(t, Config{BaseDir: base})

	_, err := Listen(Config{BaseDir: base, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err == nil {
		t.Fatal("a second endpoint claimed a pidfile that is already owned")
	}
	if !errors.Is(err, singleowner.ErrLocked) {
		t.Fatalf("error = %v, want one matching the single-owner lock sentinel", err)
	}
	// The rejection reports the holder, which means it has been near the
	// pidfile record — and the pidfile record contains the token.
	var locked *singleowner.LockedError
	if !errors.As(err, &locked) {
		t.Fatalf("error %v is not a *singleowner.LockedError", err)
	}
	token := string(first.token)
	for _, rendered := range []string{err.Error(), fmt.Sprintf("%+v", err), fmt.Sprintf("%+v", *locked)} {
		if strings.Contains(rendered, token) {
			t.Fatalf("the rejection leaked the token: %s", rendered)
		}
	}
}
