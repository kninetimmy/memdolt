package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

const (
	actionPull       = "pull_resolve"
	actionPullLegacy = "pull_resolve_legacy"
	actionPullCursor = "pull_continue"
	pullResponseID   = "pull_conflicts"
)

type repoPushInput struct {
	Remote string `json:"remote,omitempty" jsonschema:"one configured remote name; defaults to origin"`
	User   string `json:"user,omitempty" jsonschema:"SQL username override; password comes only from the executing owner's environment"`
}

type repoPullInput struct {
	Remote string `json:"remote,omitempty" jsonschema:"one configured remote name; defaults to origin"`
	User   string `json:"user,omitempty" jsonschema:"SQL username override; password comes only from the executing owner's environment"`
	Cursor string `json:"cursor,omitempty" jsonschema:"single-use continuation returned after nine conflict forms; use the same remote and user"`
}

type repoPullOutput struct {
	Result     localdolt.TransferResult `json:"result"`
	NextCursor string                   `json:"nextCursor,omitempty"`
}

type pendingPull struct {
	Input   repoPullInput
	Result  localdolt.TransferResult
	Choices []localdolt.PullChoice
	Rounds  int
}

func transferToolResult(result *localdolt.TransferResult, err error) *mcp.CallToolResult {
	response := &mcp.CallToolResult{}
	if err != nil {
		result.Error = err.Error()
		response.IsError = true
		response.Content = []mcp.Content{&mcp.TextContent{Text: result.Error}}
	}
	return response
}

func (t *Toolset) repoPush(ctx context.Context, _ *mcp.CallToolRequest, in repoPushInput) (*mcp.CallToolResult, localdolt.TransferResult, error) {
	result, err := t.store.Push(ctx, localdolt.TransferOptions{Remote: in.Remote, User: in.User, Author: ActorFromContext(ctx).CommitAuthor()})
	return transferToolResult(&result, err), result, nil
}

func (t *Toolset) repoPull(ctx context.Context, req *mcp.CallToolRequest, in repoPullInput) (*mcp.CallToolResult, repoPullOutput, error) {
	if req == nil || req.Params == nil {
		return nil, repoPullOutput{}, errors.New("repo_pull requires tool request parameters")
	}
	if in.Remote == "" {
		in.Remote = "origin"
	}
	actor := ActorFromContext(ctx)
	if req.Params.RequestState != "" || len(req.Params.InputResponses) != 0 {
		if req.Params.RequestState == "" {
			return nil, repoPullOutput{}, errors.New("repo_pull: a response without requestState approves nothing")
		}
		row, err := t.consumeElicitation(ctx, req.Params.RequestState, actor, "")
		if err != nil {
			return nil, repoPullOutput{}, err
		}
		wantAction := actionPull
		if isLegacyRequest(req) {
			wantAction = actionPullLegacy
		}
		if row.Action != wantAction || !matchesPullInput(row, in) {
			return nil, repoPullOutput{}, errors.New("repo_pull: confirmation does not match this action, remote, or user; nothing was merged")
		}
		response, err := oneElicitationResponse(req, pullResponseID)
		if err != nil {
			return nil, repoPullOutput{}, err
		}
		if response.Action != "accept" {
			row.Pull.Result.Status = response.Action
			row.Pull.Result.Remedy = "No choices were promoted. Run repo_pull again for a fresh review, or use `memdolt pull --json` in a terminal."
			return &mcp.CallToolResult{}, repoPullOutput{Result: row.Pull.Result}, nil
		}
		return t.finishPullForm(ctx, req, row, response)
	}
	if in.Cursor != "" {
		row, err := t.consumeElicitation(ctx, in.Cursor, actor, actionPullCursor)
		if err != nil {
			return nil, repoPullOutput{}, err
		}
		if !matchesPullInput(row, in) {
			return nil, repoPullOutput{}, errors.New("repo_pull continuation does not match its original remote and user")
		}
		row.Pull.Rounds = 0
		return t.issuePullForm(ctx, req, row)
	}
	result, err := t.store.Pull(ctx, localdolt.TransferOptions{Remote: in.Remote, User: in.User, Author: actor.CommitAuthor()})
	if err != nil || result.Status != "conflicted" {
		return transferToolResult(&result, err), repoPullOutput{Result: result}, nil
	}
	return t.issuePullForm(ctx, req, pendingElicitation{Actor: actor, Pull: &pendingPull{Input: in, Result: result}})
}

func matchesPullInput(row pendingElicitation, in repoPullInput) bool {
	return row.Pull != nil && row.Pull.Input.Remote == in.Remote && row.Pull.Input.User == in.User &&
		row.Position >= 0 && row.Position < len(row.Pull.Result.Conflicts) && len(row.Pull.Choices) == row.Position
}

func (t *Toolset) issuePullForm(ctx context.Context, req *mcp.CallToolRequest, row pendingElicitation) (*mcp.CallToolResult, repoPullOutput, error) {
	if row.Actor.Name == unknownActor.Name || !supportsFormElicitation(req) {
		row.Pull.Result.Remedy = "Human conflict choices require an attributed client with form elicitation. Run `memdolt pull --json`, review every row and blame, then `memdolt pull --resolve <file>` in a terminal. Main is unchanged."
		return &mcp.CallToolResult{}, repoPullOutput{Result: row.Pull.Result}, nil
	}
	row.Action = actionPull
	end := row.Position + 1
	if isLegacyRequest(req) {
		row.Action, end = actionPullLegacy, len(row.Pull.Result.Conflicts)
	}
	properties := map[string]any{}
	var required []string
	for index := row.Position; index < end; index++ {
		conflict := row.Pull.Result.Conflicts[index]
		field := fmt.Sprintf("choice_%d", index)
		properties[field] = map[string]any{"type": "string", "description": "JSON choice for " + conflict.ID + ": {\"conflict\":\"<displayed id>\",\"take\":\"ours|theirs|manual\"}. Manual includes every writable column in row (nulls included; omit generated live_key). Live-fact-key uses take=winner or manual and winner=<displayed row ID>. Task reopen requires reopen=true."}
		required = append(required, field)
	}
	shown, err := json.MarshalIndent(struct {
		LocalCommit  string                   `json:"localCommit"`
		RemoteCommit string                   `json:"remoteCommit"`
		Conflicts    []localdolt.PullConflict `json:"conflicts"`
	}{row.Pull.Result.LocalCommit, row.Pull.Result.RemoteCommit, row.Pull.Result.Conflicts[row.Position:end]}, "", "  ")
	if err != nil {
		return nil, repoPullOutput{}, err
	}
	message := fmt.Sprintf("Resolve pull conflicts %d–%d of %d. These are captured base/ours/theirs rows and real commit/blame provenance. All choices stay pending until the last accepted form validates and commits the entire merge as user. No partial promotion. Cancel, decline, expiry, restart, or stale heads leaves main unchanged. Loser facts are superseded, not deleted; task done must remain done unless you explicitly reopen it. Manual values cannot change identity or provenance.\n%s", row.Position+1, end, len(row.Pull.Result.Conflicts), shown)
	row.Pull.Rounds++
	response, _, err := t.issueElicitation(ctx, row, pullResponseID, message, map[string]any{
		"type": "object", "properties": properties, "required": required, "additionalProperties": false,
	})
	return response, repoPullOutput{}, err
}

func (t *Toolset) finishPullForm(ctx context.Context, req *mcp.CallToolRequest, row pendingElicitation, response *mcp.ElicitResult) (*mcp.CallToolResult, repoPullOutput, error) {
	end := row.Position + 1
	if row.Action == actionPullLegacy {
		end = len(row.Pull.Result.Conflicts)
	}
	if len(response.Content) != end-row.Position {
		return nil, repoPullOutput{}, errors.New("repo_pull: every displayed conflict requires exactly one complete choice; nothing was merged")
	}
	for index := row.Position; index < end; index++ {
		raw, ok := response.Content[fmt.Sprintf("choice_%d", index)].(string)
		if !ok {
			return nil, repoPullOutput{}, errors.New("repo_pull: missing or malformed conflict choice")
		}
		choice, err := localdolt.DecodePullChoice(strings.NewReader(raw))
		if err != nil {
			return nil, repoPullOutput{}, err
		}
		if choice.Conflict != row.Pull.Result.Conflicts[index].ID {
			return nil, repoPullOutput{}, errors.New("repo_pull: choice does not identify its displayed conflict")
		}
		row.Pull.Choices = append(row.Pull.Choices, choice)
	}
	row.Position = end
	if end != len(row.Pull.Result.Conflicts) {
		if row.Pull.Rounds >= maxSuccessiveReviews {
			row.Action = actionPullCursor
			cursor, err := t.mintElicitationState(ctx, row)
			if err != nil {
				return nil, repoPullOutput{}, err
			}
			row.Pull.Result.Remedy = "All choices remain pending and main is unchanged. Call repo_pull with nextCursor and the same remote/user within two minutes to reach the remaining conflicts."
			return &mcp.CallToolResult{}, repoPullOutput{Result: row.Pull.Result, NextCursor: cursor}, nil
		}
		return t.issuePullForm(ctx, req, row)
	}
	// State was atomically consumed before any response interpretation. The
	// complete shared application operation is submitted exactly once; no state
	// update after promotion can obscure the returned merge result.
	result, err := t.store.Pull(ctx, localdolt.TransferOptions{
		Remote: row.Pull.Input.Remote, User: row.Pull.Input.User,
		Author:     store.Actor{Name: "user", Email: "user@memdolt.invalid"},
		Resolution: &localdolt.PullResolution{LocalCommit: row.Pull.Result.LocalCommit, RemoteCommit: row.Pull.Result.RemoteCommit, Choices: row.Pull.Choices},
	})
	return transferToolResult(&result, err), repoPullOutput{Result: result}, nil
}
