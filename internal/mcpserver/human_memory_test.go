package mcpserver

import (
	"context"
	"fmt"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kninetimmy/memdolt/internal/memory"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

func TestHumanMemoryAppearsInMCPReadsWithoutDirectMutationTools(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%t", legacy), func(t *testing.T) {
			ctx := context.Background()
			base, st := initializedToolStore(t)
			add := func(key string) localdolt.HumanMemoryResult {
				t.Helper()
				result, err := st.FactAdd(ctx, localdolt.FactAddOptions{Fact: localdolt.Fact{Key: key, Value: "current human assertion"}, Source: "observed", Actor: memory.UserActor})
				if err != nil {
					t.Fatal(err)
				}
				return result
			}
			old, by := add("build.old"), add("build.new")
			if _, err := st.FactSupersede(ctx, old.ID, by.ID, memory.UserActor); err != nil {
				t.Fatal(err)
			}
			d, err := st.DecisionAdd(ctx, localdolt.DecisionAddOptions{Decision: localdolt.Decision{Title: "Human choice", Rationale: "human rationale", Summary: "prior summary", AlternativesRejected: "alternative", Evidence: "PR #139"}, Source: "user", Actor: memory.UserActor})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.DecisionSetSummary(ctx, d.ID, "current summary", memory.UserActor); err != nil {
				t.Fatal(err)
			}
			server := New("human-read-regression")
			toolset := RegisterTools(server, base, testElicitationBackend(base, st))
			t.Cleanup(func() { _ = toolset.Close() })
			client, session := connect(t, server, &mcp.Implementation{Name: "user", Version: "1"}, legacy)
			defer closeSessions(t, client, session)
			facts := callAs[listFactsOutput](t, client, "list_facts", map[string]any{"prefix": "build."})
			if len(facts.Facts) != 2 || facts.Facts[1].ID != old.ID || facts.Facts[1].SupersededBy != by.ID || facts.Facts[1].VerifiedAt == nil || facts.Facts[1].Stale {
				t.Fatalf("human fact reads = %+v", facts)
			}
			decisions := callAs[listDecisionsOutput](t, client, "list_decisions", map[string]any{})
			if len(decisions.Decisions) != 1 || decisions.Decisions[0].Summary != "current summary" || decisions.Decisions[0].AlternativesRejected != "alternative" || decisions.Decisions[0].Evidence != "PR #139" {
				t.Fatalf("human decision reads = %+v", decisions)
			}
			for _, name := range []string{"fact_add", "fact_verify", "decision_add", "decision_set_summary", "fact_supersede", "decision_supersede", "human_fact_add"} {
				result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: map[string]any{}})
				if err == nil && result != nil && !result.IsError {
					t.Fatalf("human operation became an MCP tool: %s", name)
				}
			}
			staged := callAs[proposeFactOutput](t, client, "propose_fact", map[string]any{"key": "agent.pending", "value": "agent claim", "rationale": "human review still required"})
			if staged.Actor.Name != "agent:user" || testText(t, st, "SELECT committer FROM dolt_log WHERE commit_hash = ?", d.Commit) != "user" {
				t.Fatalf("human/agent attribution changed: %+v", staged)
			}
			facts = callAs[listFactsOutput](t, client, "list_facts", map[string]any{})
			if len(facts.Facts) != 2 {
				t.Fatal("agent proposal became durable")
			}
		})
	}
}
