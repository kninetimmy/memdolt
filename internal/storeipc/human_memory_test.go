package storeipc_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kninetimmy/memdolt/internal/memory"
	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
	"github.com/kninetimmy/memdolt/internal/storeipc"
)

func TestHumanMemoryOwnerLostRepliesSubmitOnce(t *testing.T) {
	ctx := context.Background()
	for _, operation := range []string{"human_fact_add", "human_fact_verify", "human_fact_supersede", "human_decision_add", "human_decision_summary", "human_decision_supersede"} {
		t.Run(operation, func(t *testing.T) {
			base, st, endpoint := startOwner(t)
			if _, err := st.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			if err := endpoint.Close(); err != nil {
				t.Fatal(err)
			}
			fact, err := st.FactAdd(ctx, localdolt.FactAddOptions{Fact: localdolt.Fact{Key: "test.old", Value: "old"}, Source: "user", Actor: memory.UserActor})
			if err != nil {
				t.Fatal(err)
			}
			by, err := st.FactAdd(ctx, localdolt.FactAddOptions{Fact: localdolt.Fact{Key: "test.new", Value: "new"}, Source: "user", Actor: memory.UserActor})
			if err != nil {
				t.Fatal(err)
			}
			d, err := st.DecisionAdd(ctx, localdolt.DecisionAddOptions{Decision: localdolt.Decision{Title: "Old", Rationale: "old"}, Source: "user", Actor: memory.UserActor})
			if err != nil {
				t.Fatal(err)
			}
			dby, err := st.DecisionAdd(ctx, localdolt.DecisionAddOptions{Decision: localdolt.Decision{Title: "New", Rationale: "new"}, Source: "user", Actor: memory.UserActor})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.Commit(ctx, store.CommitRequest{Author: memory.UserActor.CommitAuthor(), NoText: true, Message: "age fixture verification", Statements: []store.Statement{
				{SQL: "UPDATE facts SET verified_at = ? WHERE id = ?", Args: []any{time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), fact.ID}},
			}}); err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			routed := transferEndpoint(t, base, st, st, func(inner http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == storeipc.OperationPath {
						raw, err := io.ReadAll(r.Body)
						if err != nil {
							t.Error(err)
							return
						}
						r.Body = io.NopCloser(bytes.NewReader(raw))
						var request struct {
							Operation string `json:"operation"`
						}
						if err := json.Unmarshal(raw, &request); err != nil {
							t.Error(err)
							return
						}
						if request.Operation == operation {
							calls.Add(1)
							inner.ServeHTTP(httptest.NewRecorder(), r)
							panic(http.ErrAbortHandler)
						}
					}
					inner.ServeHTTP(w, r)
				})
			})
			var result localdolt.HumanMemoryResult
			switch operation {
			case "human_fact_add":
				result, err = routed.FactAdd(ctx, localdolt.FactAddOptions{Fact: localdolt.Fact{Key: "test.created", Value: "confirmed"}, Source: "user", Actor: memory.UserActor})
			case "human_fact_verify":
				result, err = routed.FactVerify(ctx, fact.ID, memory.UserActor)
			case "human_fact_supersede":
				result, err = routed.FactSupersede(ctx, fact.ID, by.ID, memory.UserActor)
			case "human_decision_add":
				result, err = routed.DecisionAdd(ctx, localdolt.DecisionAddOptions{Decision: localdolt.Decision{Title: "Confirmed", Rationale: "durable"}, Source: "user", Actor: memory.UserActor})
			case "human_decision_summary":
				result, err = routed.DecisionSetSummary(ctx, d.ID, "confirmed", memory.UserActor)
			case "human_decision_supersede":
				result, err = routed.DecisionSupersede(ctx, d.ID, dby.ID, memory.UserActor)
			}
			if err == nil || !strings.Contains(err.Error(), "outcome unknown") || result.Commit != "" || calls.Load() != 1 {
				t.Fatalf("lost result=%+v, %v, calls=%d", result, err, calls.Load())
			}
			facts, err := memory.ListFacts(ctx, routed, "", 0, 90)
			if err != nil {
				t.Fatal(err)
			}
			decisions, err := memory.ListDecisions(ctx, routed, "all", 0)
			if err != nil {
				t.Fatal(err)
			}
			switch operation {
			case "human_fact_add":
				if len(facts) != 3 || facts[0].Key != "test.created" {
					t.Fatal(facts)
				}
			case "human_fact_verify":
				if facts[1].VerifiedAt == nil || facts[1].VerifiedAt.Year() == 2000 {
					t.Fatal(facts)
				}
			case "human_fact_supersede":
				if facts[1].SupersededBy != by.ID {
					t.Fatal(facts)
				}
			case "human_decision_add":
				if len(decisions) != 3 || decisions[0].Title != "Confirmed" {
					t.Fatal(decisions)
				}
			case "human_decision_summary":
				if decisions[1].Summary != "confirmed" {
					t.Fatal(decisions)
				}
			case "human_decision_supersede":
				if decisions[1].SupersededBy != dby.ID || decisions[1].Status != "superseded" {
					t.Fatal(decisions)
				}
			}
		})
	}
}

type humanMemoryFailureBackend struct{ storeipc.Backend }

func (s humanMemoryFailureBackend) FactAdd(ctx context.Context, opts localdolt.FactAddOptions) (localdolt.HumanMemoryResult, error) {
	result, err := s.Backend.FactAdd(ctx, opts)
	if err == nil {
		err = errors.New("synthetic confirmed human memory finalization failure")
	}
	return result, err
}

func TestHumanMemoryOwnerPreservesConfirmedErrorAndRefusesAgent(t *testing.T) {
	ctx := context.Background()
	base, st, endpoint := startOwner(t)
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := endpoint.Close(); err != nil {
		t.Fatal(err)
	}
	routed := transferEndpoint(t, base, st, humanMemoryFailureBackend{st}, nil)
	result, err := routed.FactAdd(ctx, localdolt.FactAddOptions{Fact: localdolt.Fact{Key: "confirmed.key", Value: "kept"}, Source: "observed", Actor: memory.UserActor})
	if err == nil || result.Commit == "" || result.Status != "created" || result.ID == "" || !strings.Contains(err.Error(), "confirmed human") {
		t.Fatalf("confirmed result lost: %+v %v", result, err)
	}
	actor, err := memory.NormalizeActor("codex")
	if err != nil {
		t.Fatal(err)
	}
	if refused, err := routed.FactVerify(ctx, result.ID, actor); err == nil || refused.Commit != "" || !strings.Contains(err.Error(), "trusted human") {
		t.Fatalf("agent used human route: %+v %v", refused, err)
	}
	if invalid, err := routed.FactAdd(ctx, localdolt.FactAddOptions{Fact: localdolt.Fact{Key: "invalid.key", Value: "\xff"}, Source: "user", Actor: memory.UserActor}); err == nil || invalid.Commit != "" || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("invalid text was rewritten by JSON: %+v %v", invalid, err)
	}
}
