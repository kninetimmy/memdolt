package ipc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

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

// How a client paces the probes one transport failure may cause.
//
// Rig 1 measured 474,234 connection-refused failures in about a minute from
// clients that redialled a dead owner without pacing, and the dial failures
// that followed (WSAENOBUFS) were caused by that hammering rather than by
// anything the store did. See docs/spikes/m0-rig1.md, F4.
const (
	// reconnectAttempts bounds the probes a single transport failure may
	// cause.
	reconnectAttempts = 5

	// reconnectBaseDelay is the pause before the second probe; each further
	// probe waits twice as long, up to reconnectMaxDelay. The five attempts
	// are therefore spaced 0, 100, 200, 400 and 400 ms: 1.1 s, which is the
	// delay schedule and nothing else.
	//
	// What a caller waits is that plus each probe's own duration, and a
	// probe against a pidfile whose lock is held makes a health request
	// bounded at probeTimeout. The worst case is therefore 1.1 s + 5 ×
	// probeTimeout — 16.1 s, measured against an owner that holds the
	// pidfile, accepts the connection and never answers.
	//
	// Only that owner costs all five attempts. The two outcomes a caller
	// actually meets are decided by the first probe with no delay at all: a
	// live owner is adopted, and a free advisory lock is already the
	// verdict that the store is unheld.
	reconnectBaseDelay = 100 * time.Millisecond
	reconnectMaxDelay  = 400 * time.Millisecond
)

// ErrNoLiveOwner reports that a transport failure was followed by a probe
// finding no live owner: either there is no pidfile, or its record is
// orphaned. The store is unheld, so the caller may open it directly (PRD
// §5.2's design response: no live server means the CLI opens the store
// itself).
//
// It says nothing about the operation that failed. That operation reached a
// transport failure, so its outcome remains unknown — an owner can commit a
// write and die before its answer arrives (docs/spikes/m0-rig1.md, F3).
var ErrNoLiveOwner = errors.New("ipc: no live owner holds the store; it may be opened directly")

// pacing spaces the probes made after a transport failure and bounds how
// many there are.
type pacing struct {
	attempts int
	base     time.Duration
	max      time.Duration
}

// delay returns the pause before attempt n, counting from 1. The first
// attempt is immediate: reading the pidfile is cheap, and a free advisory
// lock is already proof that the owner it names is gone. Each later attempt
// waits twice as long as the one before it, up to max.
func (p pacing) delay(attempt int) time.Duration {
	if attempt <= 1 {
		return 0
	}
	d := p.base
	for i := 2; i < attempt && d < p.max; i++ {
		d *= 2
	}
	if d > p.max {
		return p.max
	}
	return d
}

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
//
// A client outlives the owner it was dialled for. The endpoint it addresses
// is not fixed at Dial: after a transport failure PostJSON re-probes the
// repository's pidfile and adopts whatever live owner it finds, including a
// replacement listening on a different port with a different token. It is
// safe for concurrent use.
type Client struct {
	// baseDir is the repository whose pidfile advertises the owner. It is
	// what lets a client that lost its transport find the owner again.
	baseDir string

	// pacing and probe are fields rather than package constants so that the
	// reconnection test can count attempts and measure their spacing.
	// Production clients get the constants above and probeOwner.
	pacing pacing
	probe  func(context.Context, string) (Status, OwnerInfo, ownerDetail, error)

	// mu guards the endpoint a request is addressed to, which changes when
	// the client adopts a replacement owner.
	mu         sync.Mutex
	generation uint64
	baseURL    string
	token      secret

	// reconnecting serializes re-probing: callers that fail together probe
	// one after another rather than at once. It collapses their work only
	// when a round succeeds, because adopting an owner bumps the
	// generation and a waiter that failed on an older one then returns
	// without probing at all. A round that ends in a bounded failure
	// leaves the generation where it was, so each waiter in turn runs a
	// full round of its own: four concurrent callers against an owner that
	// accepts and never answers were measured returning after 64.4 s.
	reconnecting sync.Mutex

	// http bounds a liveness probe at probeTimeout.
	http *http.Client

	// owner serves the routes the owning process supplies through
	// Config.Handler. It sets no timeout of its own: a store operation
	// under concurrent load can legitimately take longer than a liveness
	// probe may, so its deadline belongs to the caller's context. It has a
	// connection pool of its own, so a process dials once and keeps the
	// client rather than dialling per operation — including across a
	// reconnection, which replaces the address a request is sent to and not
	// the pool it is sent over.
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
	detail, err := heldEndpoint(baseDir)
	if err != nil {
		return nil, err
	}
	return newClient(baseDir, detail), nil
}

// heldEndpoint returns the endpoint advertised by a pidfile whose lock is
// held. It reports the pidfile's state rather than the owner's liveness:
// confirming that the process on the other end is the memdolt owner it
// claims to be is Probe's job.
func heldEndpoint(baseDir string) (ownerDetail, error) {
	paths, err := layout.New(baseDir)
	if err != nil {
		return ownerDetail{}, err
	}
	owner, state, err := singleowner.Inspect(paths.PidFile())
	if err != nil {
		return ownerDetail{}, fmt.Errorf("ipc: inspect pidfile: %w", err)
	}
	if state != singleowner.StateHeld {
		return ownerDetail{}, fmt.Errorf("ipc: no live owner for %s (pidfile is %v)", baseDir, state)
	}
	_, detail, err := ownerInfoWithDetail(owner)
	if err != nil {
		return ownerDetail{}, fmt.Errorf("ipc: %w", err)
	}
	return detail, nil
}

func newClient(baseDir string, detail ownerDetail) *Client {
	c := &Client{
		baseDir: baseDir,
		pacing:  pacing{attempts: reconnectAttempts, base: reconnectBaseDelay, max: reconnectMaxDelay},
		probe:   probeOwner,
		http:    &http.Client{Timeout: probeTimeout},
		owner:   &http.Client{Transport: ownerTransport()},
	}
	c.adopt(detail)
	return c
}

// adopt points the client at an owner's endpoint.
func (c *Client) adopt(detail ownerDetail) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// The token is never part of the URL: net/http embeds the URL in
	// *url.Error, which callers log.
	c.baseURL = "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(detail.Port))
	c.token = detail.Token
	c.generation++
}

// endpoint returns the address and credential the next request is to use,
// with the generation they belong to. A caller whose request failed on
// generation g and finds the client already past g knows another caller
// re-established the transport on its behalf.
func (c *Client) endpoint() (uint64, string, secret) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.generation, c.baseURL, c.token
}

// reconnect re-establishes transport after the request sent on generation
// gen hit a transport failure.
//
// It never resubmits that request. A write over IPC is at-least-once — rig
// 1 caught an owner committing a write and dying before its answer arrived
// (docs/spikes/m0-rig1.md, F3) — and retry-safety waits for M1's
// client-minted ULIDs, so what is re-made here is the connection and
// nothing else.
//
// It ends in one of three ways: nil, meaning a live owner is now addressed
// and the next operation reaches it; an error matching ErrNoLiveOwner,
// meaning the pidfile shows the store is unheld and the caller may open it
// directly; or a bounded failure after pacing.attempts probes.
func (c *Client) reconnect(ctx context.Context, gen uint64) error {
	c.reconnecting.Lock()
	defer c.reconnecting.Unlock()

	if current, _, _ := c.endpoint(); current != gen {
		return nil
	}

	var last error
	for attempt := 1; attempt <= c.pacing.attempts; attempt++ {
		if err := wait(ctx, c.pacing.delay(attempt)); err != nil {
			return err
		}
		status, _, detail, err := c.probe(ctx, c.baseDir)
		switch {
		case err != nil:
			// Probe fails closed, so an owner that holds the pidfile but
			// does not answer yet is an error rather than a verdict. That
			// is the case the further attempts exist for.
			last = err
		case status == StatusOwnerLive:
			c.adopt(detail)
			return nil
		default:
			return fmt.Errorf("%w (pidfile is %s)", ErrNoLiveOwner, status)
		}
	}
	return fmt.Errorf("ipc: no owner answered for %s in %d attempts: %w", c.baseDir, c.pacing.attempts, last)
}

func wait(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
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
//
// A transport failure additionally re-establishes the connection, so that
// the caller's next operation reaches the current owner without the caller
// rebuilding anything. It does not resubmit this operation, whose outcome
// stays unknown; see reconnect. The error it returns reports the transport
// failure either way, and matches ErrNoLiveOwner when the re-probe found
// the store unheld.
func (c *Client) PostJSON(ctx context.Context, path string, request, response any) error {
	body, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("encode request for %s: %w", path, err)
	}

	gen, baseURL, token := c.endpoint()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request for %s: %w", path, err)
	}
	req.Header.Set(TokenHeader, string(token))
	req.Header.Set("Content-Type", "application/json")

	// net/http replays a request onto a fresh connection only when it is
	// replayable, which a POST is not unless it carries an idempotency
	// header. Nothing here sets one, and nothing here should: a write whose
	// answer was lost may already have been applied (F3), so neither this
	// package nor the transport beneath it may turn one submission into
	// two. The one replay it does make regardless — a pooled connection
	// that died before any byte of the request was written — cannot reach
	// the store twice, because the owner never saw the first attempt.
	resp, err := c.owner.Do(req)
	if err != nil {
		failed := fmt.Errorf("request %s: %w", path, err)
		// A cancelled or expired context is the caller's doing, not the
		// owner's; re-probing would only rediscover the deadline.
		if ctx.Err() != nil {
			return failed
		}
		if reconnectErr := c.reconnect(ctx, gen); reconnectErr != nil {
			return fmt.Errorf("%w: %w", failed, reconnectErr)
		}
		return failed
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
//
// It asks whichever owner the client currently addresses, which is the one
// Dial found until a reconnection adopts a replacement. It does not itself
// reconnect on a transport failure, unlike PostJSON: an unanswered health
// request is Probe's evidence, and Probe builds a client to make it, so
// recovering here would re-enter Probe through that client.
func (c *Client) Health(ctx context.Context) (Health, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	_, baseURL, token := c.endpoint()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+HealthPath, nil)
	if err != nil {
		return Health{}, fmt.Errorf("build health request: %w", err)
	}
	req.Header.Set(TokenHeader, string(token))

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
