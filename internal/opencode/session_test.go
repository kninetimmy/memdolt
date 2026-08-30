package opencode

import (
	"context"
	"errors"
	"os/exec"
	"reflect"
	"testing"
)

func TestVerifySessionValidatesBeforeInvocationAndRequiresExactInfo(t *testing.T) {
	t.Run("invalid id does not invoke a process", func(t *testing.T) {
		called := false
		_, err := verifySessionWith(context.Background(), "ses/bad", false,
			func(context.Context, string, ...string) ([]byte, error) {
				called = true
				return nil, nil
			})
		if err == nil || called {
			t.Fatalf("verifySessionWith error = %v, process called = %t; want refusal before invocation", err, called)
		}
	})

	tests := []struct {
		name string
		raw  string
	}{
		{name: "malformed JSON", raw: `{"data":`},
		{name: "missing data", raw: `{}`},
		{name: "missing id", raw: `{"data":{"agent":"build"}}`},
		{name: "mismatched id", raw: `{"data":{"id":"ses_other"}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := verifySessionWith(context.Background(), "ses_current", false,
				func(context.Context, string, ...string) ([]byte, error) {
					return []byte(tc.raw), nil
				})
			if err == nil {
				t.Fatal("invalid Session.Info was accepted")
			}
		})
	}
}

func TestVerifySessionReturnsAvailableProvenanceAndUsesArguments(t *testing.T) {
	var program string
	var args []string
	info, err := verifySessionWith(context.Background(), "ses_current-1", false,
		func(_ context.Context, gotProgram string, gotArgs ...string) ([]byte, error) {
			program = gotProgram
			args = append([]string(nil), gotArgs...)
			return []byte(`{
  "data": {
    "id": "ses_current-1",
    "agent": "build",
    "model": {"providerID": "openai", "id": "gpt-5.6", "variant": "xhigh"}
  }
}`), nil
		})
	if err != nil {
		t.Fatalf("verify session: %v", err)
	}
	if program != "opencode2" || !reflect.DeepEqual(args,
		[]string{"api", "get", "/api/session/ses_current-1"}) {
		t.Fatalf("invocation = %q %q, want argument-based opencode2 api get", program, args)
	}
	want := SessionInfo{
		SessionID: "ses_current-1", AgentID: "build", ProviderID: "openai",
		ModelID: "gpt-5.6", Variant: "xhigh",
	}
	if !reflect.DeepEqual(info, want) {
		t.Fatalf("SessionInfo = %+v, want %+v", info, want)
	}
}

func TestVerifySessionWindowsFallbackIsNotAnExitFailureFallback(t *testing.T) {
	t.Run("bare unavailable", func(t *testing.T) {
		var programs []string
		info, err := verifySessionWith(context.Background(), "ses_current", true,
			func(_ context.Context, program string, _ ...string) ([]byte, error) {
				programs = append(programs, program)
				if program == "opencode2" {
					return nil, exec.ErrNotFound
				}
				return []byte(`{"data":{"id":"ses_current"}}`), nil
			})
		if err != nil || info.SessionID != "ses_current" {
			t.Fatalf("fallback result = %+v, %v", info, err)
		}
		if !reflect.DeepEqual(programs, []string{"opencode2", "opencode2.cmd"}) {
			t.Fatalf("programs = %q, want bare then .cmd", programs)
		}
	})

	t.Run("bare exits nonzero", func(t *testing.T) {
		var programs []string
		_, err := verifySessionWith(context.Background(), "ses_current", true,
			func(_ context.Context, program string, _ ...string) ([]byte, error) {
				programs = append(programs, program)
				return nil, errors.New("exit status 1")
			})
		if err == nil {
			t.Fatal("failed bare command was accepted")
		}
		if !reflect.DeepEqual(programs, []string{"opencode2"}) {
			t.Fatalf("programs = %q, .cmd must not mask a bare command failure", programs)
		}
	})
}

func TestValidateSessionIDShape(t *testing.T) {
	for _, id := range []string{"ses_a", "sesABC-123", "ses0"} {
		if err := ValidateSessionID(id); err != nil {
			t.Errorf("valid id %q rejected: %v", id, err)
		}
	}
	for _, id := range []string{"", "ses", "session/1", "ses.dot", "ses space", "sesé"} {
		if err := ValidateSessionID(id); err == nil {
			t.Errorf("invalid id %q accepted", id)
		}
	}
}
