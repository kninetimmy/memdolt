package mcpserver

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kninetimmy/memdolt/internal/memory"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

const (
	requestStateTTL        = 2 * time.Minute
	requestStateBytes      = 32
	maxSuccessiveReviews   = 9 // the SDK permits at most nine input rounds per call
	reviewResponseID       = "review"
	factConflictResponseID = "fact_conflict"

	actionReviewOne    = "review_accept_one"
	actionReviewLegacy = "review_accept_legacy"
	actionReviewBatch  = "review_accept_batch"
	actionReviewCursor = "review_continue"
	actionFactConflict = "propose_fact_conflict"
)

// pendingElicitation is the server-side row behind one opaque requestState.
// It is deliberately session-local: expiry or server restart invalidates the
// capability, and no ephemeral approval material enters Dolt history or the
// derived embedding side-store.
type pendingElicitation struct {
	Repository  string
	Actor       memory.Actor
	ProposalIDs []string
	// ProposalCommits is parallel to ProposalIDs and binds every approval to
	// the exact staging commit its dialog rendered.
	ProposalCommits []string
	Position        int
	Action          string
	ExpiresAt       time.Time
	Accepted        []localdolt.AcceptResult
	Skipped         []string
	Fact            *pendingFactConflict
	NextCursor      string
}

type pendingFactConflict struct {
	Input   proposeFactInput
	Current localdolt.FactSnapshot
}

type reviewPendingInput struct {
	Mode   string `json:"mode,omitempty" jsonschema:"successive (default) or batch"`
	Cursor string `json:"cursor,omitempty" jsonschema:"single-use continuation returned after nine modern successive rounds"`
}

type reviewFailure struct {
	ProposalID string `json:"proposalId"`
	Error      string `json:"error"`
}

type reviewPendingOutput struct {
	Mode          string                   `json:"mode"`
	Status        string                   `json:"status"`
	Accepted      []localdolt.AcceptResult `json:"accepted"`
	Skipped       []string                 `json:"skipped"`
	Failures      []reviewFailure          `json:"failures"`
	RepoPending   int                      `json:"repoPending"`
	GlobalPending int                      `json:"globalPending"`
	NextCursor    string                   `json:"nextCursor,omitempty"`
	Remedy        string                   `json:"remedy,omitempty"`
}

func (t *Toolset) reviewPending(ctx context.Context, req *mcp.CallToolRequest, in reviewPendingInput) (*mcp.CallToolResult, reviewPendingOutput, error) {
	actor := ActorFromContext(ctx)
	if req == nil || req.Params == nil {
		return nil, reviewPendingOutput{}, errors.New("review_pending: missing tool request parameters")
	}
	if req.Params.RequestState == "" && len(req.Params.InputResponses) == 0 {
		mode, action, err := reviewMode(in.Mode)
		if err != nil {
			return nil, reviewPendingOutput{}, err
		}
		if actor.Name == unknownActor.Name {
			return nil, reviewPendingOutput{}, errors.New("review_pending requires attributed clientInfo; use `memdolt review` in a terminal")
		}
		if in.Cursor != "" {
			if mode != "successive" {
				return nil, reviewPendingOutput{}, errors.New("review_pending: a continuation cursor is valid only in successive mode")
			}
			return t.continueReview(ctx, req, actor, mode, in.Cursor)
		}
		if action == actionReviewOne && isLegacyRequest(req) {
			action = actionReviewLegacy
		}
		return t.startReview(ctx, req, actor, mode, action)
	}
	if req.Params.RequestState == "" {
		return nil, reviewPendingOutput{}, errors.New("review_pending: an elicitation response without requestState cannot approve anything")
	}

	row, err := t.consumeElicitation(ctx, req.Params.RequestState, actor, "")
	if err != nil {
		return nil, reviewPendingOutput{}, fmt.Errorf("review_pending: %w", err)
	}
	mode, action, err := reviewMode(in.Mode)
	if err != nil {
		return nil, reviewPendingOutput{}, err
	}
	if action == actionReviewOne && isLegacyRequest(req) {
		action = actionReviewLegacy
	}
	if row.Action != action {
		return nil, reviewPendingOutput{}, errors.New("review_pending: requestState does not match the requested review action; nothing was approved")
	}
	response, err := oneElicitationResponse(req, reviewResponseID)
	if err != nil {
		return nil, reviewPendingOutput{}, fmt.Errorf("review_pending: %w", err)
	}
	if response.Action == "decline" || response.Action == "cancel" {
		return t.reviewTerminal(ctx, mode, response.Action, row, nil)
	}
	if action == actionReviewLegacy {
		return t.finishLegacySuccessiveReview(ctx, mode, row, response)
	}
	decision, err := responseChoice(response, "decision")
	if err != nil {
		return nil, reviewPendingOutput{}, fmt.Errorf("review_pending: %w", err)
	}
	if len(response.Content) != 1 {
		return nil, reviewPendingOutput{}, errors.New("review_pending: malformed elicitation response fields; nothing was approved")
	}
	if action == actionReviewBatch {
		return t.finishBatchReview(ctx, mode, row, decision)
	}
	return t.finishSuccessiveReview(ctx, mode, row, decision)
}

func isLegacyRequest(req *mcp.CallToolRequest) bool {
	return req.ProtocolVersion() < modernProtocolVersion
}

func (t *Toolset) continueReview(
	ctx context.Context,
	req *mcp.CallToolRequest,
	actor memory.Actor,
	mode, cursor string,
) (*mcp.CallToolResult, reviewPendingOutput, error) {
	row, err := t.consumeElicitation(ctx, cursor, actor, actionReviewCursor)
	if err != nil {
		return nil, reviewPendingOutput{}, fmt.Errorf("review_pending: %w", err)
	}
	if !supportsFormElicitation(req) {
		return t.reviewTerminal(ctx, mode, "cli_required", row, nil)
	}
	row.Action = actionReviewOne
	result, _, err := t.issueSuccessiveReview(ctx, row)
	return result, reviewPendingOutput{}, err
}

func reviewMode(raw string) (mode, action string, err error) {
	mode = strings.TrimSpace(raw)
	if mode == "" {
		mode = "successive"
	}
	switch mode {
	case "successive":
		return mode, actionReviewOne, nil
	case "batch":
		return mode, actionReviewBatch, nil
	default:
		return "", "", fmt.Errorf("review mode %q must be successive or batch", mode)
	}
}

func (t *Toolset) startReview(ctx context.Context, req *mcp.CallToolRequest, actor memory.Actor, mode, action string) (*mcp.CallToolResult, reviewPendingOutput, error) {
	pending, err := t.store.PendingProposals(ctx)
	if err != nil {
		return nil, reviewPendingOutput{}, err
	}
	ids := make([]string, 0, len(pending))
	commits := make([]string, 0, len(pending))
	for _, proposal := range pending {
		if proposal.Target == localdolt.TargetRepo {
			ids = append(ids, proposal.ID)
			commits = append(commits, proposal.Commit)
		}
	}
	if len(ids) == 0 {
		return t.reviewTerminal(ctx, mode, "empty", pendingElicitation{Actor: actor}, nil)
	}
	if !supportsFormElicitation(req) {
		return t.reviewTerminal(ctx, mode, "cli_required", pendingElicitation{Actor: actor}, nil)
	}
	row := pendingElicitation{
		Actor: actor, ProposalIDs: ids, ProposalCommits: commits, Position: 0, Action: action,
		Accepted: []localdolt.AcceptResult{}, Skipped: []string{},
	}
	if mode == "batch" {
		result, _, err := t.issueBatchReview(ctx, row)
		return result, reviewPendingOutput{}, err
	}
	if action == actionReviewLegacy {
		result, _, err := t.issueLegacySuccessiveReview(ctx, row)
		return result, reviewPendingOutput{}, err
	}
	result, _, err := t.issueSuccessiveReview(ctx, row)
	return result, reviewPendingOutput{}, err
}

func (t *Toolset) issueSuccessiveReview(ctx context.Context, row pendingElicitation) (*mcp.CallToolResult, string, error) {
	if row.Position < 0 || row.Position >= len(row.ProposalIDs) {
		return nil, "", errors.New("review_pending: invalid queue position")
	}
	diff, err := t.proposalDiffAtSnapshot(ctx, row, row.Position)
	if err != nil {
		return nil, "", err
	}
	encoded, err := json.MarshalIndent(diff, "", "  ")
	if err != nil {
		return nil, "", fmt.Errorf("review_pending: encode proposal: %w", err)
	}
	message := fmt.Sprintf("Repository proposal %d of %d:\n%s", row.Position+1, len(row.ProposalIDs), encoded)
	schema := choiceSchema("decision", []string{"approve", "skip", "cancel"}, "Approve this proposal, leave it pending and continue, or stop review.")
	return t.issueElicitation(ctx, row, reviewResponseID, message, schema)
}

func (t *Toolset) issueBatchReview(ctx context.Context, row pendingElicitation) (*mcp.CallToolResult, string, error) {
	diffs := make([]localdolt.ProposalDiff, 0, len(row.ProposalIDs))
	for position := range row.ProposalIDs {
		diff, err := t.proposalDiffAtSnapshot(ctx, row, position)
		if err != nil {
			return nil, "", err
		}
		diffs = append(diffs, diff)
	}
	encoded, err := json.MarshalIndent(diffs, "", "  ")
	if err != nil {
		return nil, "", fmt.Errorf("review_pending: encode proposal batch: %w", err)
	}
	message := fmt.Sprintf(
		"Approve these %d repository proposals in order. Each successful proposal lands immediately; a later guard refusal stops the batch without undoing earlier approvals:\n%s",
		len(diffs), encoded)
	schema := choiceSchema("decision", []string{"approve_all", "cancel"}, "Approve the shown proposals sequentially, allowing partial progress if a later guard refuses, or cancel before any approval.")
	return t.issueElicitation(ctx, row, reviewResponseID, message, schema)
}

func (t *Toolset) issueLegacySuccessiveReview(ctx context.Context, row pendingElicitation) (*mcp.CallToolResult, string, error) {
	diffs := make([]localdolt.ProposalDiff, 0, len(row.ProposalIDs))
	properties := make(map[string]any, len(row.ProposalIDs))
	required := make([]string, 0, len(row.ProposalIDs))
	for position, id := range row.ProposalIDs {
		diff, err := t.proposalDiffAtSnapshot(ctx, row, position)
		if err != nil {
			return nil, "", err
		}
		diffs = append(diffs, diff)
		properties[id] = map[string]any{
			"type": "string", "enum": []any{"approve", "skip"},
			"description": "Approve this exact proposal or leave it pending.",
		}
		required = append(required, id)
	}
	encoded, err := json.MarshalIndent(diffs, "", "  ")
	if err != nil {
		return nil, "", fmt.Errorf("review_pending: encode legacy proposal review: %w", err)
	}
	message := fmt.Sprintf(
		"Legacy one-round successive review for %d repository proposals. Choose approve or skip for every proposal; approved entries are applied oldest-first and a later guard refusal does not undo earlier approvals:\n%s",
		len(diffs), encoded)
	schema := map[string]any{
		"type": "object", "properties": properties, "required": required, "additionalProperties": false,
	}
	return t.issueElicitation(ctx, row, reviewResponseID, message, schema)
}

func (t *Toolset) proposalDiffAtSnapshot(ctx context.Context, row pendingElicitation, position int) (localdolt.ProposalDiff, error) {
	if position < 0 || position >= len(row.ProposalIDs) || len(row.ProposalCommits) != len(row.ProposalIDs) {
		return localdolt.ProposalDiff{}, errors.New("review_pending: invalid proposal snapshot")
	}
	id, commit := row.ProposalIDs[position], row.ProposalCommits[position]
	diff, err := t.store.ProposalDiff(ctx, id)
	if err != nil {
		return localdolt.ProposalDiff{}, err
	}
	if diff.Proposal.Target != localdolt.TargetRepo || diff.Proposal.ID != id || diff.Proposal.Commit != commit {
		return localdolt.ProposalDiff{}, fmt.Errorf(
			"review_pending: proposal %s no longer matches shown commit %s; nothing was approved", id, commit)
	}
	return diff, nil
}

func (t *Toolset) finishSuccessiveReview(ctx context.Context, mode string, row pendingElicitation, decision string) (*mcp.CallToolResult, reviewPendingOutput, error) {
	if row.Position < 0 || row.Position >= len(row.ProposalIDs) || len(row.ProposalCommits) != len(row.ProposalIDs) {
		return nil, reviewPendingOutput{}, errors.New("review_pending: invalid queue position in requestState")
	}
	if decision == "cancel" {
		return t.reviewTerminal(ctx, mode, "canceled", row, nil)
	}
	if decision != "approve" && decision != "skip" {
		return nil, reviewPendingOutput{}, fmt.Errorf("review_pending: decision %q is not approve, skip, or cancel", decision)
	}

	var next *mcp.CallToolResult
	var nextState string
	var pageBoundary bool
	if row.Position+1 < len(row.ProposalIDs) {
		nextRow := row
		nextRow.Position++
		var err error
		if nextRow.Position%maxSuccessiveReviews == 0 {
			pageBoundary = true
			nextRow.Action = actionReviewCursor
			nextState, err = t.mintElicitationState(ctx, nextRow)
		} else {
			next, nextState, err = t.issueSuccessiveReview(ctx, nextRow)
		}
		if err != nil {
			return nil, reviewPendingOutput{}, err
		}
	}

	id := row.ProposalIDs[row.Position]
	if decision == "approve" {
		accepted, err := t.store.ReviewAcceptExpected(
			ctx, id, row.ProposalCommits[row.Position], memory.UserActor.CommitAuthor(), false)
		if err != nil {
			discardErr := t.discardElicitation(ctx, nextState)
			if discardErr != nil {
				err = errors.Join(err, discardErr)
			}
			return t.reviewTerminal(ctx, mode, "blocked", row, []reviewFailure{{ProposalID: id, Error: err.Error()}})
		}
		row.Accepted = append(row.Accepted, accepted)
	} else {
		row.Skipped = append(row.Skipped, id)
	}
	if nextState != "" {
		updated, err := t.updateElicitationProgress(ctx, nextState, row.Accepted, row.Skipped)
		if err != nil {
			return t.reviewTerminal(ctx, mode, "stopped", row, []reviewFailure{{ProposalID: id, Error: err.Error()}})
		}
		if !updated {
			return t.reviewTerminal(ctx, mode, "stopped", row, []reviewFailure{{ProposalID: id, Error: "continuation state expired before it could be returned"}})
		}
		if pageBoundary {
			row.NextCursor = nextState
			return t.reviewTerminal(ctx, mode, "partial", row, nil)
		}
		return next, reviewPendingOutput{}, nil
	}
	return t.reviewTerminal(ctx, mode, "complete", row, nil)
}

func (t *Toolset) finishBatchReview(ctx context.Context, mode string, row pendingElicitation, decision string) (*mcp.CallToolResult, reviewPendingOutput, error) {
	if decision == "cancel" {
		return t.reviewTerminal(ctx, mode, "canceled", row, nil)
	}
	if decision != "approve_all" {
		return nil, reviewPendingOutput{}, fmt.Errorf("review_pending: decision %q is not approve_all or cancel", decision)
	}
	// Resolve every id before the first merge. A missing, reordered, or
	// global-target row makes the batch stale and therefore changes nothing.
	for position, id := range row.ProposalIDs {
		_, err := t.proposalDiffAtSnapshot(ctx, row, position)
		if err != nil {
			return t.reviewTerminal(ctx, mode, "stale", row, []reviewFailure{{ProposalID: id, Error: fmt.Sprintf("queue position %d: %v", position, err)}})
		}
	}
	for position, id := range row.ProposalIDs {
		accepted, err := t.store.ReviewAcceptExpected(
			ctx, id, row.ProposalCommits[position], memory.UserActor.CommitAuthor(), false)
		if err != nil {
			return t.reviewTerminal(ctx, mode, "blocked", row, []reviewFailure{{ProposalID: id, Error: err.Error()}})
		}
		row.Accepted = append(row.Accepted, accepted)
	}
	return t.reviewTerminal(ctx, mode, "complete", row, nil)
}

func (t *Toolset) finishLegacySuccessiveReview(
	ctx context.Context,
	mode string,
	row pendingElicitation,
	response *mcp.ElicitResult,
) (*mcp.CallToolResult, reviewPendingOutput, error) {
	if response.Action != "accept" || response.Content == nil ||
		len(row.ProposalIDs) == 0 || len(row.ProposalCommits) != len(row.ProposalIDs) ||
		len(response.Content) != len(row.ProposalIDs) {
		return nil, reviewPendingOutput{}, errors.New("review_pending: incomplete legacy review response; nothing was approved")
	}
	decisions := make([]string, len(row.ProposalIDs))
	for position, id := range row.ProposalIDs {
		decision, ok := response.Content[id].(string)
		if !ok || decision != "approve" && decision != "skip" {
			return nil, reviewPendingOutput{}, fmt.Errorf(
				"review_pending: legacy response for proposal %s must be approve or skip; nothing was approved", id)
		}
		decisions[position] = decision
		if _, err := t.proposalDiffAtSnapshot(ctx, row, position); err != nil {
			return t.reviewTerminal(ctx, mode, "stale", row, []reviewFailure{{ProposalID: id, Error: err.Error()}})
		}
	}
	for position, id := range row.ProposalIDs {
		if decisions[position] == "skip" {
			row.Skipped = append(row.Skipped, id)
			continue
		}
		accepted, err := t.store.ReviewAcceptExpected(
			ctx, id, row.ProposalCommits[position], memory.UserActor.CommitAuthor(), false)
		if err != nil {
			return t.reviewTerminal(ctx, mode, "blocked", row, []reviewFailure{{ProposalID: id, Error: err.Error()}})
		}
		row.Accepted = append(row.Accepted, accepted)
	}
	return t.reviewTerminal(ctx, mode, "complete", row, nil)
}

func (t *Toolset) reviewTerminal(ctx context.Context, mode, status string, row pendingElicitation, failures []reviewFailure) (*mcp.CallToolResult, reviewPendingOutput, error) {
	pending, err := t.store.PendingProposals(ctx)
	if err != nil {
		return nil, reviewPendingOutput{}, err
	}
	var repo, global int
	for _, proposal := range pending {
		if proposal.Target == localdolt.TargetGlobal {
			global++
		} else {
			repo++
		}
	}
	remedy := ""
	if status == "cli_required" && repo > 0 {
		remedy = fmt.Sprintf(
			"%d repository and %d global proposals pending — this client lacks form elicitation; run `memdolt review` in a terminal.",
			repo, global)
	} else if global > 0 {
		remedy = fmt.Sprintf("%d global proposals pending — run `memdolt review` in a terminal.", global)
	}
	if row.Accepted == nil {
		row.Accepted = []localdolt.AcceptResult{}
	}
	if row.Skipped == nil {
		row.Skipped = []string{}
	}
	if failures == nil {
		failures = []reviewFailure{}
	}
	return &mcp.CallToolResult{}, reviewPendingOutput{
		Mode: mode, Status: status, Accepted: row.Accepted, Skipped: row.Skipped,
		Failures: failures, RepoPending: repo, GlobalPending: global, NextCursor: row.NextCursor, Remedy: remedy,
	}, nil
}

func (t *Toolset) proposeFactWithConflict(ctx context.Context, req *mcp.CallToolRequest, in proposeFactInput) (*mcp.CallToolResult, proposeFactOutput, error) {
	actor := ActorFromContext(ctx)
	if req == nil || req.Params == nil {
		return nil, proposeFactOutput{}, errors.New("propose_fact: missing tool request parameters")
	}
	if req.Params.RequestState != "" || len(req.Params.InputResponses) != 0 {
		if req.Params.RequestState == "" {
			return nil, proposeFactOutput{}, errors.New("propose_fact: an elicitation response without requestState cannot stage anything")
		}
		row, err := t.consumeElicitation(ctx, req.Params.RequestState, actor, actionFactConflict)
		if err != nil {
			return nil, proposeFactOutput{}, fmt.Errorf("propose_fact: %w", err)
		}
		return t.finishFactConflict(ctx, req, in, row)
	}

	current, err := t.liveFactByKey(ctx, in.Key)
	if err != nil {
		return nil, proposeFactOutput{}, err
	}
	if current == nil {
		staged, err := t.store.ProposeFact(ctx, proposal(actor, in.Rationale, in.Global), factFromInput(in))
		if errors.Is(err, localdolt.ErrFactKeyExists) {
			current, _ = t.liveFactByKey(ctx, in.Key)
		} else {
			return &mcp.CallToolResult{}, factOutput(staged, actor), err
		}
	}
	if current == nil {
		return nil, proposeFactOutput{}, fmt.Errorf("propose_fact: fact key %q collided but its live row could not be read", in.Key)
	}
	if in.Global {
		return nil, proposeFactOutput{}, fmt.Errorf("propose_fact: fact key %q: %w; global promotion remains CLI-only", in.Key, localdolt.ErrFactKeyExists)
	}
	if !supportsFormElicitation(req) {
		return nil, proposeFactOutput{}, fmt.Errorf("propose_fact: fact key %q: %w; this client does not support elicitation", in.Key, localdolt.ErrFactKeyExists)
	}
	encoded, err := json.MarshalIndent(struct {
		Current  localdolt.FactSnapshot `json:"current"`
		Proposed proposeFactInput       `json:"proposed"`
	}{Current: *current, Proposed: in}, "", "  ")
	if err != nil {
		return nil, proposeFactOutput{}, err
	}
	row := pendingElicitation{
		Actor: actor, Action: actionFactConflict, Position: 0,
		Fact: &pendingFactConflict{Input: in, Current: *current},
	}
	schema := choiceSchema("action", []string{"overwrite", "supersede", "keep_both", "cancel"}, "How to resolve the live fact-key collision.")
	properties := schema["properties"].(map[string]any)
	properties["new_key"] = map[string]any{"type": "string", "description": "Required only for keep_both: a distinct dotted fact key."}
	result, _, err := t.issueElicitation(ctx, row, factConflictResponseID,
		"A live fact already uses this key. Choose how to stage the proposed claim for later review:\n"+string(encoded), schema)
	return result, proposeFactOutput{}, err
}

func (t *Toolset) finishFactConflict(ctx context.Context, req *mcp.CallToolRequest, in proposeFactInput, row pendingElicitation) (*mcp.CallToolResult, proposeFactOutput, error) {
	if row.Fact == nil || row.Fact.Input != in {
		return nil, proposeFactOutput{}, errors.New("propose_fact: requestState does not match the exact proposed fact")
	}
	response, err := oneElicitationResponse(req, factConflictResponseID)
	if err != nil {
		return nil, proposeFactOutput{}, fmt.Errorf("propose_fact: %w", err)
	}
	if response.Action == "decline" || response.Action == "cancel" {
		return &mcp.CallToolResult{}, proposeFactOutput{Actor: row.Actor, Resolution: response.Action, Canceled: true}, nil
	}
	action, err := responseChoice(response, "action")
	if err != nil {
		return nil, proposeFactOutput{}, fmt.Errorf("propose_fact: %w", err)
	}
	if action == "cancel" {
		return &mcp.CallToolResult{}, proposeFactOutput{Actor: row.Actor, Resolution: action, Canceled: true}, nil
	}
	p := proposal(row.Actor, in.Rationale, false)
	fact := factFromInput(in)
	resolution := localdolt.FactResolution(action)
	switch action {
	case "overwrite":
		if _, exists := response.Content["new_key"]; exists {
			return nil, proposeFactOutput{}, errors.New("propose_fact: new_key is valid only with keep_both")
		}
	case "supersede":
		if _, exists := response.Content["new_key"]; exists {
			return nil, proposeFactOutput{}, errors.New("propose_fact: new_key is valid only with keep_both")
		}
	case "keep_both":
		newKey, ok := response.Content["new_key"].(string)
		if !ok || !validDistinctDottedKey(newKey, in.Key) {
			return nil, proposeFactOutput{}, errors.New("propose_fact: keep_both requires a distinct dotted new_key with no empty segment")
		}
		fact.Key = newKey
	default:
		return nil, proposeFactOutput{}, fmt.Errorf("propose_fact: action %q is not overwrite, supersede, keep_both, or cancel", action)
	}
	staged, err := t.store.ProposeFactResolution(ctx, p, row.Fact.Current, fact, resolution)
	output := factOutput(staged, row.Actor)
	output.Resolution = action
	return &mcp.CallToolResult{}, output, err
}

func factOutput(staged localdolt.StagedProposal, actor memory.Actor) proposeFactOutput {
	return proposeFactOutput{
		ID: staged.ID, Branch: staged.Branch, Kind: staged.Kind,
		RowID: staged.RowID, Commit: staged.Commit, Actor: actor,
	}
}

func factFromInput(in proposeFactInput) localdolt.Fact {
	return localdolt.Fact{Key: in.Key, Value: in.Value, Kind: in.Kind, Evidence: in.Evidence}
}

func validDistinctDottedKey(key, old string) bool {
	if key == old || key != strings.TrimSpace(key) || !strings.Contains(key, ".") {
		return false
	}
	for _, part := range strings.Split(key, ".") {
		if part == "" {
			return false
		}
	}
	return true
}

func (t *Toolset) liveFactByKey(ctx context.Context, key string) (_ *localdolt.FactSnapshot, err error) {
	rows, err := t.store.Query(ctx, "SELECT id, `key`, value, source, kind, evidence, verified_at, created_at, superseded_by "+
		"FROM facts AS OF 'main' WHERE live_key = ?", key)
	if err != nil {
		return nil, fmt.Errorf("look up live fact key %q: %w", key, err)
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, nil
	}
	var fact localdolt.FactSnapshot
	var factKey, value, source, kind, evidence, superseded sql.NullString
	var verified, created sql.NullTime
	if err := rows.Scan(&fact.ID, &factKey, &value, &source, &kind, &evidence, &verified, &created, &superseded); err != nil {
		return nil, err
	}
	fact.Key, fact.Value = factKey.String, value.String
	fact.Source = nullableStringPointer(source)
	fact.Kind = nullableStringPointer(kind)
	fact.Evidence = nullableStringPointer(evidence)
	fact.SupersededBy = nullableStringPointer(superseded)
	if verified.Valid {
		stamp := verified.Time
		fact.VerifiedAt = &stamp
	}
	if created.Valid {
		fact.CreatedAt = created.Time
	}
	if rows.Next() {
		return nil, fmt.Errorf("fact key %q has more than one live row", key)
	}
	return &fact, rows.Err()
}

func nullableStringPointer(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	return &value.String
}

func supportsFormElicitation(req *mcp.CallToolRequest) bool {
	capabilities := req.ClientCapabilities()
	if capabilities == nil || capabilities.Elicitation == nil {
		return false
	}
	// The protocol treats an empty elicitation capability as form support for
	// compatibility. A URL-only declaration is not form support.
	return capabilities.Elicitation.Form != nil || capabilities.Elicitation.URL == nil
}

func oneElicitationResponse(req *mcp.CallToolRequest, id string) (*mcp.ElicitResult, error) {
	if req == nil || req.Params == nil || len(req.Params.InputResponses) == 0 {
		return nil, errors.New("missing elicitation response; nothing was approved")
	}
	if len(req.Params.InputResponses) != 1 {
		return nil, errors.New("malformed elicitation response set; nothing was approved")
	}
	raw, ok := req.Params.InputResponses[id]
	if !ok || raw == nil {
		return nil, fmt.Errorf("missing %q elicitation response; nothing was approved", id)
	}
	response, ok := raw.(*mcp.ElicitResult)
	if !ok || response == nil {
		return nil, fmt.Errorf("malformed %q elicitation response; nothing was approved", id)
	}
	switch response.Action {
	case "accept", "decline", "cancel":
		return response, nil
	default:
		return nil, fmt.Errorf("malformed elicitation action %q; nothing was approved", response.Action)
	}
}

func responseChoice(response *mcp.ElicitResult, field string) (string, error) {
	if response.Action != "accept" || response.Content == nil {
		return "", errors.New("incomplete elicitation response; nothing was approved")
	}
	value, ok := response.Content[field].(string)
	if !ok || strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("incomplete elicitation response: %s is required; nothing was approved", field)
	}
	for key := range response.Content {
		if key != field && key != "new_key" {
			return "", fmt.Errorf("malformed elicitation response field %q; nothing was approved", key)
		}
	}
	return value, nil
}

func choiceSchema(field string, choices []string, description string) map[string]any {
	enum := make([]any, len(choices))
	for i, choice := range choices {
		enum[i] = choice
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			field: map[string]any{"type": "string", "enum": enum, "description": description},
		},
		"required":             []string{field},
		"additionalProperties": false,
	}
}

func (t *Toolset) issueElicitation(ctx context.Context, row pendingElicitation, id, message string, schema map[string]any) (*mcp.CallToolResult, string, error) {
	state, err := t.mintElicitationState(ctx, row)
	if err != nil {
		return nil, "", err
	}
	return &mcp.CallToolResult{
		InputRequests: mcp.InputRequestMap{id: &mcp.ElicitParams{Message: message, RequestedSchema: schema}},
		RequestState:  state,
	}, state, nil
}

func (t *Toolset) mintElicitationState(ctx context.Context, row pendingElicitation) (string, error) {
	if t.elicitErr != nil {
		return "", fmt.Errorf("elicitation state storage is unavailable: %w", t.elicitErr)
	}
	row.Repository = t.store.DataDir()
	row.ExpiresAt = time.Now().UTC().Add(requestStateTTL)
	random := make([]byte, requestStateBytes)
	if _, err := io.ReadFull(rand.Reader, random); err != nil {
		return "", fmt.Errorf("mint cryptographic requestState: %w", err)
	}
	state := base64.RawURLEncoding.EncodeToString(random)
	if err := t.elicit.insert(ctx, state, row); err != nil {
		return "", err
	}
	return state, nil
}

func (t *Toolset) consumeElicitation(ctx context.Context, state string, actor memory.Actor, action string) (pendingElicitation, error) {
	if t.elicitErr != nil {
		return pendingElicitation{}, fmt.Errorf("elicitation state storage is unavailable: %w", t.elicitErr)
	}
	row, err := t.elicit.consume(ctx, state)
	if err != nil {
		return pendingElicitation{}, fmt.Errorf("%w; nothing was approved", err)
	}
	if !row.ExpiresAt.After(time.Now().UTC()) {
		return pendingElicitation{}, errors.New("requestState expired; nothing was approved")
	}
	if row.Repository != t.store.DataDir() || row.Actor != actor || (action != "" && row.Action != action) {
		return pendingElicitation{}, errors.New("requestState does not match this repository, client, or action; nothing was approved")
	}
	return row, nil
}

func (t *Toolset) discardElicitation(ctx context.Context, state string) error {
	if t.elicitErr != nil {
		return fmt.Errorf("elicitation state storage is unavailable: %w", t.elicitErr)
	}
	return t.elicit.discard(ctx, state)
}

func (t *Toolset) updateElicitationProgress(ctx context.Context, state string, accepted []localdolt.AcceptResult, skipped []string) (bool, error) {
	if t.elicitErr != nil {
		return false, fmt.Errorf("elicitation state storage is unavailable: %w", t.elicitErr)
	}
	return t.elicit.updateProgress(ctx, state, accepted, skipped)
}

func (t *Toolset) closeElicitationState() error {
	if t.elicitErr != nil {
		return t.elicitErr
	}
	if t.elicit == nil {
		return nil
	}
	return t.elicit.Close()
}
