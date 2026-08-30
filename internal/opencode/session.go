// Package opencode verifies identity supplied by an OpenCode host session.
package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

// SessionInfo is the verified OpenCode provenance a wrap-up note persists.
type SessionInfo struct {
	SessionID  string `json:"session_id"`
	AgentID    string `json:"agent_id,omitempty"`
	ProviderID string `json:"provider_id,omitempty"`
	ModelID    string `json:"model_id,omitempty"`
	Variant    string `json:"variant,omitempty"`
}

type apiResponse struct {
	Data *struct {
		ID    string `json:"id"`
		Agent string `json:"agent"`
		Model *struct {
			ProviderID string `json:"providerID"`
			ID         string `json:"id"`
			Variant    string `json:"variant"`
		} `json:"model"`
	} `json:"data"`
}

type commandRunner func(context.Context, string, ...string) ([]byte, error)

// ValidateSessionID applies §11.4's allowlist before the id can enter a
// process argument. A valid id is "ses" plus a nonempty ASCII suffix made of
// letters, digits, underscores, or hyphens.
func ValidateSessionID(id string) error {
	suffix, ok := strings.CutPrefix(id, "ses")
	if !ok || suffix == "" {
		return fmt.Errorf("invalid OpenCode session id %q: expected ses followed by an ASCII identifier", id)
	}
	for i := range len(suffix) {
		c := suffix[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '_' || c == '-' {
			continue
		}
		return fmt.Errorf("invalid OpenCode session id %q: suffix contains a disallowed character", id)
	}
	return nil
}

// VerifySession independently fetches Session.Info for the host-supplied id
// and requires the response to name that exact id.
func VerifySession(ctx context.Context, id string) (SessionInfo, error) {
	return verifySessionWith(ctx, id, runtime.GOOS == "windows", runCommand)
}

func verifySessionWith(ctx context.Context, id string, windows bool, run commandRunner) (SessionInfo, error) {
	if err := ValidateSessionID(id); err != nil {
		return SessionInfo{}, err
	}

	args := []string{"api", "get", "/api/session/" + id}
	raw, err := run(ctx, "opencode2", args...)
	if err != nil && windows && errors.Is(err, exec.ErrNotFound) {
		raw, err = run(ctx, "opencode2.cmd", args...)
	}
	if err != nil {
		return SessionInfo{}, fmt.Errorf("verify OpenCode session with opencode2: %w", err)
	}

	var response apiResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return SessionInfo{}, fmt.Errorf("decode OpenCode Session.Info: %w", err)
	}
	if response.Data == nil || response.Data.ID == "" {
		return SessionInfo{}, errors.New("OpenCode Session.Info response has no data.id")
	}
	if response.Data.ID != id {
		return SessionInfo{}, fmt.Errorf("OpenCode Session.Info id mismatch: requested %q, got %q", id, response.Data.ID)
	}

	info := SessionInfo{SessionID: response.Data.ID, AgentID: response.Data.Agent}
	if response.Data.Model != nil {
		info.ProviderID = response.Data.Model.ProviderID
		info.ModelID = response.Data.Model.ID
		info.Variant = response.Data.Model.Variant
	}
	return info, nil
}

func runCommand(ctx context.Context, program string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, program, args...)
	out, err := cmd.Output()
	if err == nil {
		return out, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
		return nil, fmt.Errorf("%s failed: %w: %s", program, err, strings.TrimSpace(string(exitErr.Stderr)))
	}
	return nil, fmt.Errorf("%s failed: %w", program, err)
}
