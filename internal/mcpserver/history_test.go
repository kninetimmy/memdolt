package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kninetimmy/memdolt/internal/memory"
	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

func TestHistoryMCPParityNullableSchemasAndQueuePreservation(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%t", legacy), func(t *testing.T) {
			ctx := context.Background()
			base, st := initializedToolStore(t)
			commit, err := st.Commit(ctx, store.CommitRequest{Author: memory.UserActor.CommitAuthor(), Message: "history MCP fixture", NoText: true, Statements: []store.Statement{
				{SQL: "INSERT INTO facts (id, `key`, value, source) VALUES ('f', 'history.mcp', NULL, '')"},
				{SQL: "INSERT INTO decisions (id, title, rationale, status) VALUES ('d', NULL, '', NULL)"},
				{SQL: "INSERT INTO project_state (id, body, actor_raw) VALUES ('s', NULL, '')"},
				{SQL: "INSERT INTO project_arch (id, body, created_at) VALUES ('a', 'old imported architecture', '2000-01-01 00:00:00')"},
			}})
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 26; i++ {
				if _, _, err := memory.New(st, memory.UserActor).SetNarrative(ctx, memory.StateNarrative, fmt.Sprintf("state %d", i)); err != nil {
					t.Fatal(err)
				}
			}
			pending, err := st.ProposeFact(ctx, localdolt.Proposal{Rationale: "history exclusion", Actor: memory.UserActor.CommitAuthor(), Target: localdolt.TargetRepo}, localdolt.Fact{Key: "history.pending", Value: "UNACCEPTED"})
			if err != nil {
				t.Fatal(err)
			}
			server := New("test")
			tools := registerTools(server, base, testElicitationBackend(base, st), time.Hour)
			client, session := connect(t, server, &mcp.Implementation{Name: "history-client", Version: "1"}, legacy)
			callOK(t, client, "log_session_note", map[string]any{"text": "pending MCP history control"})
			before := testText(t, st, "SELECT CONCAT(DOLT_HASHOF('main'), DOLT_HASHOF_DB('WORKING'), DOLT_HASHOF_DB('STAGED'))")
			for _, subject := range []string{"fact", "decision", "state", "arch"} {
				opts := localdolt.HistoryOptions{Subject: subject, Limit: 25}
				args := map[string]any{"subject": subject}
				if subject == "fact" || subject == "decision" {
					id := subject[:1]
					opts.ID, args["id"] = &id, id
				}
				for _, past := range []bool{false, true} {
					if past {
						opts.AsOf, args["as_of"] = &commit.Hash, commit.Hash
					}
					want, err := st.History(ctx, opts)
					if err != nil {
						t.Fatal(err)
					}
					got := callAs[localdolt.HistoryResult](t, client, "history", args)
					wantJSON, marshalErr := json.Marshal(want)
					gotJSON, gotErr := json.Marshal(got)
					if marshalErr != nil || gotErr != nil || string(gotJSON) != string(wantJSON) {
						t.Fatalf("MCP %s parity = %s, want %s (%v %v)", subject, gotJSON, wantJSON, marshalErr, gotErr)
					}
					if subject == "state" && !past && len(got.Changes) != 25 {
						t.Fatal("omitted MCP limit did not use 25")
					}
				}
			}
			for _, args := range []map[string]any{
				{"subject": "state", "limit": 0}, {"subject": "arch", "limit": 200001},
				{"subject": "state", "id": ""}, {"subject": "fact"}, {"subject": "task"},
				{"subject": "arch", "as_of": ""}, {"subject": "arch", "as_of": "main"},
				{"subject": "fact", "id": pending.RowID, "as_of": pending.Commit},
			} {
				callError(t, client, "history", args, "history")
			}
			missing := callAs[localdolt.HistoryResult](t, client, "history", map[string]any{"subject": "decision", "id": "absent"})
			if missing.Current != nil || missing.Blame != nil || missing.Changes == nil || len(missing.Changes) != 0 {
				t.Fatal("missing row output was fabricated")
			}
			if after := testText(t, st, "SELECT CONCAT(DOLT_HASHOF('main'), DOLT_HASHOF_DB('WORKING'), DOLT_HASHOF_DB('STAGED'))"); after != before || testCount(t, st, "SELECT COUNT(*) FROM session_notes") != 0 {
				t.Fatal("MCP history changed roots or flushed notes")
			}
			tools.mu.Lock()
			pendingNotes := len(tools.groups) == 1 && len(tools.groups[0].notes) == 1
			tools.mu.Unlock()
			if !pendingNotes {
				t.Fatal("history changed the MCP accumulator")
			}
			callOK(t, client, "status", map[string]any{})
			closeSessions(t, client, session)
			if err := tools.Close(); err != nil {
				t.Fatal(err)
			}
			if testCount(t, st, "SELECT COUNT(*) FROM session_notes") != 1 {
				t.Fatal("ordinary shutdown did not retain the queued note")
			}
		})
	}
}
