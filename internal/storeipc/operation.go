package storeipc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/kninetimmy/memdolt/internal/memory"
	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

// OperationPath carries the typed store operations used by shipped CLI
// surfaces that cannot be expressed through Store.Commit or Store.Query.
const OperationPath = "/v1/store/operation"

const (
	opSchemaVersion    = "schema_version"
	opEmbeddingSources = "embedding_sources"
	opRecallSources    = "recall_sources"
	opRecallFTS        = "recall_fts"
	opSearchDecisions  = "search_decisions"
	opLastChanged      = "last_changed"
	opCheckWriteText   = "check_write_text"
	opRecordCommand    = "record_command"
	opProposeFact      = "propose_fact"
	opResolveFact      = "propose_fact_resolution"
	opProposeDecision  = "propose_decision"
	opProposeSupersede = "propose_supersede"
	opPendingProposals = "pending_proposals"
	opProposalDiff     = "proposal_diff"
	opRejectProposal   = "reject_proposal"
	opExpireProposals  = "expire_proposals"
	opReviewAccept     = "review_accept"
)

// Backend is the initialized data-store surface the live owner exposes. The
// application-level review accept gate is supplied separately, so this
// interface cannot accidentally reduce promotion to a raw storage call. The
// operation handler is an explicit allow-list over both surfaces.
type Backend interface {
	store.Store
	SchemaVersion(context.Context) (int, error)
	EmbeddingSources(context.Context) ([]store.EmbeddingSource, error)
	RecallSources(context.Context) ([]store.RecallSource, error)
	RecallFTS(context.Context, string, []string) ([]store.LexicalHit, error)
	SearchDecisions(context.Context, string, int) ([]store.DecisionSearchHit, error)
	LastChanged(context.Context, string, string) (*store.CommitProvenance, error)
	CheckWriteText(context.Context, []string) error
	ProposeFact(context.Context, localdolt.Proposal, localdolt.Fact) (localdolt.StagedProposal, error)
	ProposeFactResolution(context.Context, localdolt.Proposal, localdolt.FactSnapshot, localdolt.Fact, localdolt.FactResolution) (localdolt.StagedProposal, error)
	ProposeDecision(context.Context, localdolt.Proposal, localdolt.Decision) (localdolt.StagedProposal, error)
	ProposeSupersede(context.Context, localdolt.Proposal, string, localdolt.Fact) (localdolt.StagedProposal, error)
	PendingProposals(context.Context) ([]localdolt.PendingProposal, error)
	ProposalDiff(context.Context, string) (localdolt.ProposalDiff, error)
	RejectProposal(context.Context, string) (localdolt.PendingProposal, error)
	ExpireProposals(context.Context, time.Time) ([]localdolt.PendingProposal, error)
}

var _ Backend = (*localdolt.Store)(nil)

type operationRequest struct {
	Operation string          `json:"operation"`
	Args      json.RawMessage `json:"args,omitempty"`
}

type recallFTSArgs struct {
	Query       string   `json:"query"`
	SourceTypes []string `json:"sourceTypes"`
}

type searchDecisionsArgs struct {
	Query string `json:"query"`
	Limit int    `json:"limit"`
}

type lastChangedArgs struct {
	SourceType string `json:"sourceType"`
	SourceID   string `json:"sourceId"`
}

type checkWriteTextArgs struct {
	Text []string `json:"text"`
}

type recordCommandArgs struct {
	Actor    memory.Actor `json:"actor"`
	Kind     string       `json:"kind"`
	Cmdline  string       `json:"cmdline"`
	ExitCode int          `json:"exitCode"`
}

type recordCommandResult struct {
	Command memory.Command `json:"command"`
	Commit  string         `json:"commit"`
}

type proposeFactArgs struct {
	Proposal localdolt.Proposal `json:"proposal"`
	Fact     localdolt.Fact     `json:"fact"`
}

type resolveFactArgs struct {
	Proposal    localdolt.Proposal       `json:"proposal"`
	Expected    localdolt.FactSnapshot   `json:"expected"`
	Replacement localdolt.Fact           `json:"replacement"`
	Resolution  localdolt.FactResolution `json:"resolution"`
}

type proposeDecisionArgs struct {
	Proposal localdolt.Proposal `json:"proposal"`
	Decision localdolt.Decision `json:"decision"`
}

type proposeSupersedeArgs struct {
	Proposal     localdolt.Proposal `json:"proposal"`
	SupersededID string             `json:"supersededId"`
	Replacement  localdolt.Fact     `json:"replacement"`
}

type proposalIDArgs struct {
	ID string `json:"id"`
}

type expireProposalsArgs struct {
	Before time.Time `json:"before"`
}

type acceptProposalArgs struct {
	ID             string      `json:"id"`
	ExpectedCommit string      `json:"expectedCommit,omitempty"`
	Reviewer       store.Actor `json:"reviewer"`
	Force          bool        `json:"force,omitempty"`
}

// ReviewAcceptFunc is the application review gate an owner exposes. The
// production functions are internal/review.Accept and AcceptExpected; keeping
// it explicit prevents the transport from falling back to a raw storage merge
// that omits commit binding, contradiction configuration, force semantics, or
// future application guards.
type ReviewAcceptFunc func(context.Context, string, string, store.Actor, bool) (localdolt.AcceptResult, error)

func (h *handler) handleOperation(w http.ResponseWriter, r *http.Request) {
	var req operationRequest
	if !h.decode(w, r, &req) {
		return
	}
	if req.Operation == "" {
		h.fail(w, http.StatusBadRequest, errors.New("store operation is required"))
		return
	}

	ctx := r.Context()
	var result any
	var err error
	switch req.Operation {
	case opSchemaVersion:
		result, err = h.store.SchemaVersion(ctx)
	case opEmbeddingSources:
		result, err = h.store.EmbeddingSources(ctx)
	case opRecallSources:
		result, err = h.store.RecallSources(ctx)
	case opRecallFTS:
		args, decodeErr := operationArgs[recallFTSArgs](req.Args)
		if decodeErr != nil {
			err = decodeErr
			break
		}
		result, err = h.store.RecallFTS(ctx, args.Query, args.SourceTypes)
	case opSearchDecisions:
		args, decodeErr := operationArgs[searchDecisionsArgs](req.Args)
		if decodeErr != nil {
			err = decodeErr
			break
		}
		result, err = h.store.SearchDecisions(ctx, args.Query, args.Limit)
	case opLastChanged:
		args, decodeErr := operationArgs[lastChangedArgs](req.Args)
		if decodeErr != nil {
			err = decodeErr
			break
		}
		result, err = h.store.LastChanged(ctx, args.SourceType, args.SourceID)
	case opCheckWriteText:
		args, decodeErr := operationArgs[checkWriteTextArgs](req.Args)
		if decodeErr != nil {
			err = decodeErr
			break
		}
		err = h.store.CheckWriteText(ctx, args.Text)
		result = struct{}{}
	case opRecordCommand:
		args, decodeErr := operationArgs[recordCommandArgs](req.Args)
		if decodeErr != nil {
			err = decodeErr
			break
		}
		command, commit, recordErr := memory.New(h.store, args.Actor).RecordCommand(
			ctx, args.Kind, args.Cmdline, args.ExitCode)
		result, err = recordCommandResult{Command: command, Commit: commit}, recordErr
	case opProposeFact:
		args, decodeErr := operationArgs[proposeFactArgs](req.Args)
		if decodeErr != nil {
			err = decodeErr
			break
		}
		result, err = h.store.ProposeFact(ctx, args.Proposal, args.Fact)
	case opResolveFact:
		args, decodeErr := operationArgs[resolveFactArgs](req.Args)
		if decodeErr != nil {
			err = decodeErr
			break
		}
		result, err = h.store.ProposeFactResolution(ctx, args.Proposal, args.Expected, args.Replacement, args.Resolution)
	case opProposeDecision:
		args, decodeErr := operationArgs[proposeDecisionArgs](req.Args)
		if decodeErr != nil {
			err = decodeErr
			break
		}
		result, err = h.store.ProposeDecision(ctx, args.Proposal, args.Decision)
	case opProposeSupersede:
		args, decodeErr := operationArgs[proposeSupersedeArgs](req.Args)
		if decodeErr != nil {
			err = decodeErr
			break
		}
		result, err = h.store.ProposeSupersede(ctx, args.Proposal, args.SupersededID, args.Replacement)
	case opPendingProposals:
		result, err = h.store.PendingProposals(ctx)
	case opProposalDiff:
		args, decodeErr := operationArgs[proposalIDArgs](req.Args)
		if decodeErr != nil {
			err = decodeErr
			break
		}
		result, err = h.store.ProposalDiff(ctx, args.ID)
	case opRejectProposal:
		args, decodeErr := operationArgs[proposalIDArgs](req.Args)
		if decodeErr != nil {
			err = decodeErr
			break
		}
		result, err = h.store.RejectProposal(ctx, args.ID)
	case opExpireProposals:
		args, decodeErr := operationArgs[expireProposalsArgs](req.Args)
		if decodeErr != nil {
			err = decodeErr
			break
		}
		result, err = h.store.ExpireProposals(ctx, args.Before)
	case opReviewAccept:
		args, decodeErr := operationArgs[acceptProposalArgs](req.Args)
		if decodeErr != nil {
			err = decodeErr
			break
		}
		result, err = h.reviewAccept(ctx, args.ID, args.ExpectedCommit, args.Reviewer, args.Force)
	default:
		h.fail(w, http.StatusNotFound, fmt.Errorf("unknown store operation %q", req.Operation))
		return
	}
	if err != nil {
		h.fail(w, http.StatusInternalServerError, err)
		return
	}
	h.respond(w, result)
}

func operationArgs[T any](raw json.RawMessage) (T, error) {
	var args T
	if len(raw) == 0 {
		return args, errors.New("store operation arguments are required")
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return args, fmt.Errorf("decode store operation arguments: %w", err)
	}
	return args, nil
}
