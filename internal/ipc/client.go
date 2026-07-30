package ipc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/kninetimmy/memdolt/internal/layout"
	"github.com/kninetimmy/memdolt/internal/singleowner"
)

// maxErrorBodyBytes bounds how much of a rejected request's answer is kept
// in the error. The owner is trusted, but an error value travels, and an
// unbounded one is a denial-of-service on whatever logs it.
const maxErrorBodyBytes = 512

// ownerMaxIdleConns is how many idle connections a client keeps to its
// owner.
//
// net/http keeps two per host by default, which is wrong for this endpoint
// in a way that only shows up under load: a caller making more than two
// concurrent requests gets a fresh TCP connection for each one, every one
// of those connections lands in TIME_WAIT, and on Windows the ephemeral
// port range runs out. Measured in PRD §16's rig 1 at 268,912 requests
// failing with WSAEADDRINUSE over five minutes before this was set — none
// of which reached the store, so none of which lost data, but all of which
// were failures the store never had a chance to answer. See
// docs/spikes/m0-rig1.md.
const ownerMaxIdleConns = 64

// StatusError reports that the owner answered with a status other than
// 200 OK.
//
// It is deliberately distinguishable from a transport failure. A status
// means the owner received the request and refused or failed it, so the
// caller knows the operation did not happen; a transport failure leaves
// the outcome unknown, because the request may have been executed and only
// the answer lost. Callers that must not double-count a write depend on
// that distinction.
type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("owner returned status %d", e.Code)
	}
	return fmt.Sprintf("owner returned status %d: %s", e.Code, e.Body)
}

// Client talks to the process that owns a repository's store.
type Client struct {
	baseURL string
	token   secret

	// http bounds a liveness probe at probeTimeout.
	http *http.Client

	// owner serves the routes the owning process supplies through
	// Config.Handler. It sets no timeout of its own: a store operation
	// under concurrent load can legitimately take longer than a liveness
	// probe may, so its deadline belongs to the caller's context. It has a
	// connection pool of its own, so a process dials once and keeps the
	// client rather than dialling per operation.
	owner *http.Client
}

// ownerTransport returns the connection pool a client keeps to its owner.
func ownerTransport() *http.Transport {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Transport{MaxIdleConnsPerHost: ownerMaxIdleConns}
	}
	pool := transport.Clone()
	pool.MaxIdleConns = ownerMaxIdleConns
	pool.MaxIdleConnsPerHost = ownerMaxIdleConns
	return pool
}

// Dial reads the repository's pidfile and returns a client for the owner
// it names. It fails if no live process holds the pidfile.
func Dial(baseDir string) (*Client, error) {
	paths, err := layout.New(baseDir)
	if err != nil {
		return nil, err
	}
	owner, state, err := singleowner.Inspect(paths.PidFile())
	if err != nil {
		return nil, fmt.Errorf("ipc: inspect pidfile: %w", err)
	}
	if state != singleowner.StateHeld {
		return nil, fmt.Errorf("ipc: no live owner for %s (pidfile is %v)", baseDir, state)
	}
	_, detail, err := ownerInfoWithDetail(owner)
	if err != nil {
		return nil, fmt.Errorf("ipc: %w", err)
	}
	return newClient(detail), nil
}

func newClient(detail ownerDetail) *Client {
	return &Client{
		// The token is never part of the URL: net/http embeds the URL in
		// *url.Error, which callers log.
		baseURL: "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(detail.Port)),
		token:   detail.Token,
		http:    &http.Client{Timeout: probeTimeout},
		owner:   &http.Client{Transport: ownerTransport()},
	}
}

// PostJSON sends request as JSON to one of the routes the owning process
// serves through Config.Handler, and decodes the answer into response,
// which may be nil.
//
// It exists so that the token stays inside this package: a caller that
// needs to reach an owner-served route asks for the route, never for the
// credential. path is a route and must not carry one either — a query
// string ends up inside *url.Error, which callers routinely log.
//
// A non-2xx answer is returned as *StatusError; anything else is a
// transport failure, and the two mean different things to a caller that
// has to decide whether a write happened.
func (c *Client) PostJSON(ctx context.Context, path string, request, response any) error {
	body, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("encode request for %s: %w", path, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request for %s: %w", path, err)
	}
	req.Header.Set(TokenHeader, string(c.token))
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.owner.Do(req)
	if err != nil {
		return fmt.Errorf("request %s: %w", path, err)
	}
	// Draining before closing is what lets net/http put the connection back
	// in the pool. A body closed with bytes still in it costs a connection
	// per request, which is the same exhaustion ownerMaxIdleConns is set
	// for.
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		return &StatusError{Code: resp.StatusCode, Body: strings.TrimSpace(string(detail))}
	}
	if response == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(response); err != nil {
		return fmt.Errorf("decode response from %s: %w", path, err)
	}
	return nil
}

// Health asks the owner to identify itself.
func (c *Client) Health(ctx context.Context) (Health, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+HealthPath, nil)
	if err != nil {
		return Health{}, fmt.Errorf("build health request: %w", err)
	}
	req.Header.Set(TokenHeader, string(c.token))

	resp, err := c.http.Do(req)
	if err != nil {
		return Health{}, fmt.Errorf("health request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return Health{}, fmt.Errorf("health request: unexpected status %s", resp.Status)
	}

	var health Health
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		return Health{}, fmt.Errorf("decode health response: %w", err)
	}
	return health, nil
}
