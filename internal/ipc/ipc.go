// Package ipc implements the loopback endpoint that lets a memdolt CLI
// reach the process which owns the embedded store (PRD §5.2.1).
//
// When the MCP server is running it is the sole holder of the Dolt data
// directory. A CLI invocation therefore has to find out whether an owner
// exists before it tries to open the store itself: Probe answers that with
// three outcomes — a live owner, an owner that died and left its pidfile
// behind, or no owner at all — and a CLI routes through the endpoint or
// opens the store directly accordingly.
//
// # Shape of the endpoint
//
// The owner binds 127.0.0.1 on an ephemeral port and records the port, its
// pid and a per-run token in <base>/.memdolt/server.pid, created 0600. The
// pidfile doubles as the owner's single-owner lock (see
// internal/singleowner), so an owner that is killed rather than shut down
// releases it at the kernel level and Probe can tell that its pidfile is
// orphaned without having to trust a pid.
//
// # Token handling
//
// The token is 32 bytes from crypto/rand, compared in constant time, and
// required on every request including the health check. It travels in the
// X-Memdolt-Token header and never in the URL: a token in a query string
// ends up inside *url.Error, which callers routinely log. Nothing in this
// package writes a token to a log or an error, and the type it is held in
// redacts itself when formatted, so an accidental %v cannot leak it
// either.
package ipc

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/kninetimmy/memdolt/internal/layout"
	"github.com/kninetimmy/memdolt/internal/singleowner"
)

const (
	// TokenHeader carries the per-run token on every request.
	TokenHeader = "X-Memdolt-Token"

	// HealthPath is the endpoint's own route. It reports the owner's
	// identity so that a prober can confirm it is talking to the process
	// named in the pidfile.
	HealthPath = "/v0/health"

	// loopbackAddr binds the loopback interface explicitly on an ephemeral
	// port. It is never ":0", which would also accept off-machine traffic.
	loopbackAddr = "127.0.0.1:0"

	tokenBytes          = 32
	readHeaderTimeout   = 5 * time.Second
	shutdownGracePeriod = 2 * time.Second
)

// secret holds a credential. It renders as "[redacted]" through fmt and
// slog so that a stray %v, %+v or log attribute cannot print it. JSON
// encoding is unaffected, which is what lets it round-trip through the
// pidfile.
type secret string

func (secret) String() string { return "[redacted]" }

// GoString implements fmt.GoStringer, which is what %#v consults —
// including for a secret reached through a struct field.
func (secret) GoString() string { return `"[redacted]"` }

// LogValue implements slog.LogValuer.
func (secret) LogValue() slog.Value { return slog.StringValue("[redacted]") }

// ownerDetail is the memdolt-specific half of the pidfile record; the
// generic half (pid, host, owner id, timestamps) belongs to
// internal/singleowner.
type ownerDetail struct {
	Port  int    `json:"port"`
	Token secret `json:"token"`
}

// Health is the health endpoint's response. It deliberately carries no
// token.
type Health struct {
	OK        bool      `json:"ok"`
	PID       int       `json:"pid"`
	OwnerID   string    `json:"ownerId"`
	StartedAt time.Time `json:"startedAt"`
}

// Config configures the endpoint.
type Config struct {
	// BaseDir is the repository whose store this process owns. The
	// pidfile is <BaseDir>/.memdolt/server.pid (PRD §5.3).
	BaseDir string

	// Handler serves routes other than the health check. It is subject to
	// the same token check. It may be nil, which is the M0 case: the
	// endpoint exists to be found and proved live, and the milestone that
	// routes store operations over it supplies the routes.
	Handler http.Handler

	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// Server is a running loopback endpoint and the pidfile that advertises it.
type Server struct {
	paths     layout.Paths
	logger    *slog.Logger
	token     secret
	lock      *singleowner.Lock
	listener  net.Listener
	http      *http.Server
	startedAt time.Time

	serveErr  chan error
	closeOnce sync.Once
	closeErr  error
}

// Listen binds the loopback endpoint and writes the pidfile.
//
// If another live process already owns this repository's pidfile, Listen
// fails with an error matching errors.Is(err, singleowner.ErrLocked). If
// the pidfile was left behind by a process that died, its record is
// cleared and the removal is logged loudly before Listen proceeds.
func Listen(cfg Config) (*Server, error) {
	paths, err := layout.New(cfg.BaseDir)
	if err != nil {
		return nil, err
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	if err := os.MkdirAll(paths.Dir(), 0o755); err != nil {
		return nil, fmt.Errorf("ipc: create %s: %w", paths.Dir(), err)
	}

	token, err := newToken()
	if err != nil {
		return nil, fmt.Errorf("ipc: generate token: %w", err)
	}

	listener, err := net.Listen("tcp", loopbackAddr)
	if err != nil {
		return nil, fmt.Errorf("ipc: listen on %s: %w", loopbackAddr, err)
	}

	port, err := listenerPort(listener)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("ipc: %w", err), listener.Close())
	}

	// The port has to be known before the pidfile can advertise it, so the
	// listener is bound first. A process that loses the race for the
	// pidfile gives its ephemeral port straight back.
	lock, err := singleowner.Acquire(paths.PidFile(), singleowner.Options{
		Detail: ownerDetail{Port: port, Token: token},
		Logger: logger,
	})
	if err != nil {
		return nil, errors.Join(fmt.Errorf("ipc: claim pidfile: %w", err), listener.Close())
	}

	s := &Server{
		paths:     paths,
		logger:    logger,
		token:     token,
		lock:      lock,
		listener:  listener,
		startedAt: time.Now().UTC(),
		serveErr:  make(chan error, 1),
	}
	s.http = &http.Server{
		Handler: s.routes(cfg.Handler),
		// net/http's own error log never prints request headers, so it
		// cannot leak the token; ReadHeaderTimeout is set because the
		// endpoint accepts connections from anything running as this user.
		ReadHeaderTimeout: readHeaderTimeout,
	}

	go func() {
		err := s.http.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		s.serveErr <- err
	}()

	logger.Info("memdolt: single-owner ipc endpoint listening",
		"addr", listener.Addr().String(), "pid", os.Getpid(), "pidfile", paths.PidFile())
	return s, nil
}

// Addr returns the loopback address the endpoint is bound to.
func (s *Server) Addr() string { return s.listener.Addr().String() }

// Port returns the ephemeral port the endpoint is bound to.
func (s *Server) Port() int {
	port, err := listenerPort(s.listener)
	if err != nil {
		return 0
	}
	return port
}

// PidFile returns the path of the pidfile this endpoint owns.
func (s *Server) PidFile() string { return s.paths.PidFile() }

// OwnerID returns the per-acquisition identity recorded in the pidfile and
// reported by the health endpoint.
func (s *Server) OwnerID() string { return s.lock.Owner().ID }

// Close stops serving and releases the pidfile. It is idempotent, and
// repeat calls report the same result as the first.
func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), shutdownGracePeriod)
		defer cancel()

		var errs []error
		if err := s.http.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("ipc: shut down endpoint: %w", err))
		}
		if err := <-s.serveErr; err != nil {
			errs = append(errs, fmt.Errorf("ipc: serve: %w", err))
		}
		if err := s.lock.Release(); err != nil {
			errs = append(errs, fmt.Errorf("ipc: release pidfile: %w", err))
		}
		s.closeErr = errors.Join(errs...)
	})
	return s.closeErr
}

func (s *Server) routes(extra http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(HealthPath, s.handleHealth)
	if extra != nil {
		mux.Handle("/", extra)
	}
	return s.requireToken(mux)
}

// requireToken rejects every request that does not present the per-run
// token. The rejection says nothing about what was presented or expected.
func (s *Server) requireToken(next http.Handler) http.Handler {
	expected := []byte(s.token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented := []byte(r.Header.Get(TokenHeader))
		if len(presented) == 0 || subtle.ConstantTimeCompare(presented, expected) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := json.Marshal(Health{
		OK:        true,
		PID:       os.Getpid(),
		OwnerID:   s.lock.Owner().ID,
		StartedAt: s.startedAt,
	})
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(body); err != nil {
		s.logger.Warn("memdolt: ipc health response write failed", "error", err.Error())
	}
}

func newToken() (secret, error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return secret(hex.EncodeToString(buf)), nil
}

func listenerPort(listener net.Listener) (int, error) {
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("listener address %q is not a TCP address", listener.Addr())
	}
	return addr.Port, nil
}
