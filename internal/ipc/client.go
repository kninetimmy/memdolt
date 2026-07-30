package ipc

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"

	"github.com/kninetimmy/memdolt/internal/layout"
	"github.com/kninetimmy/memdolt/internal/singleowner"
)

// Client talks to the process that owns a repository's store.
type Client struct {
	baseURL string
	token   secret
	http    *http.Client
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
	}
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
