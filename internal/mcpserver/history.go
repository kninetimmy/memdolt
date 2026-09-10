package mcpserver

import (
	"context"
	"reflect"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

func historyTool() *mcp.Tool {
	// The SDK's existing schema package infers maps as non-null objects.
	// Native absence is a null row, so override only this tool's row-map type;
	// retain inferred commit fields and nullable cells from the shared DTO.
	schema, err := jsonschema.For[localdolt.HistoryResult](&jsonschema.ForOptions{
		TypeSchemas: map[reflect.Type]*jsonschema.Schema{
			reflect.TypeFor[map[string]*string](): {Types: []string{"object", "null"}, AdditionalProperties: &jsonschema.Schema{Types: []string{"string", "null"}}},
		},
	})
	if err != nil {
		panic(err) // Invalid static tool schemas also fail registration in AddTool.
	}
	return &mcp.Tool{Name: "history", OutputSchema: schema, Description: "Inspect committed fact/decision row changes by explicit id, or the state/arch table timeline. Return exact nullable before/after images, native commit/parent evidence, current values and blame. Optional as_of accepts only a full Dolt hash in captured main ancestry; dirty and unaccepted memory are excluded. Never flush queued notes."}
}

type historyInput struct {
	Subject string  `json:"subject" jsonschema:"fact, decision, state or arch"`
	ID      *string `json:"id,omitempty" jsonschema:"required explicit row id for fact/decision; omit for state/arch"`
	AsOf    *string `json:"as_of,omitempty" jsonschema:"optional full Dolt commit hash in captured main ancestry"`
	Limit   *int    `json:"limit,omitempty" jsonschema:"maximum change rows, 1 through 200000; omission uses 25"`
}

func (t *Toolset) history(ctx context.Context, _ *mcp.CallToolRequest, in historyInput) (*mcp.CallToolResult, localdolt.HistoryResult, error) {
	limit := defaultListLimit
	if in.Limit != nil {
		limit = *in.Limit
	}
	result, err := t.store.History(ctx, localdolt.HistoryOptions{Subject: in.Subject, ID: in.ID, AsOf: in.AsOf, Limit: limit})
	return &mcp.CallToolResult{}, result, err
}
