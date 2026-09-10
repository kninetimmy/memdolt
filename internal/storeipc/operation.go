package storeipc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/kninetimmy/memdolt/internal/memory"
	"github.com/kninetimmy/memdolt/internal/render"
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
	opHistory          = "history"
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
	opPush             = "push"
	opPull             = "pull"
	opListRemotes      = "list_remotes"
	opAddRemote        = "add_remote"
	opDocAdd           = "doc_add"
	opDocList          = "doc_list"
	opDocShow          = "doc_show"
	opDocRemove        = "doc_remove"
	opRender           = "render"
	opRepoStatus       = "repo_status"
	opExportMemory     = "export_memory"
	opImportMemory     = "import_memory"
	opCapturePromotion = "capture_promotion"
	opCaptureRecall    = "capture_recall"
)

const (
	opFactAdd           = "human_fact_add"
	opFactVerify        = "human_fact_verify"
	opFactSupersede     = "human_fact_supersede"
	opDecisionAdd       = "human_decision_add"
	opDecisionSummary   = "human_decision_summary"
	opDecisionSupersede = "human_decision_supersede"
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
	CapturePromotion(context.Context, string, string) (localdolt.PromotionRecord, error)
	CaptureRecall(context.Context, store.RecallSnapshotOptions) (store.RecallSnapshot, error)
	SearchDecisions(context.Context, string, int) ([]store.DecisionSearchHit, error)
	LastChanged(context.Context, string, string) (*store.CommitProvenance, error)
	History(context.Context, localdolt.HistoryOptions) (localdolt.HistoryResult, error)
	CheckWriteText(context.Context, []string) error
	ProposeFact(context.Context, localdolt.Proposal, localdolt.Fact) (localdolt.StagedProposal, error)
	ProposeFactResolution(context.Context, localdolt.Proposal, localdolt.FactSnapshot, localdolt.Fact, localdolt.FactResolution) (localdolt.StagedProposal, error)
	ProposeDecision(context.Context, localdolt.Proposal, localdolt.Decision) (localdolt.StagedProposal, error)
	ProposeSupersede(context.Context, localdolt.Proposal, string, localdolt.Fact) (localdolt.StagedProposal, error)
	PendingProposals(context.Context) ([]localdolt.PendingProposal, error)
	ProposalDiff(context.Context, string) (localdolt.ProposalDiff, error)
	RejectProposal(context.Context, string) (localdolt.PendingProposal, error)
	ExpireProposals(context.Context, time.Time) ([]localdolt.PendingProposal, error)
	Push(context.Context, localdolt.TransferOptions) (localdolt.TransferResult, error)
	Pull(context.Context, localdolt.TransferOptions) (localdolt.TransferResult, error)
	ListRemotes(context.Context) ([]localdolt.Remote, error)
	AddRemote(context.Context, localdolt.Remote) (localdolt.Remote, error)
	DocAdd(context.Context, localdolt.DocAddOptions) (localdolt.DocResult, error)
	DocList(context.Context) ([]localdolt.Document, error)
	DocShow(context.Context, string) (localdolt.DocResult, error)
	DocRemove(context.Context, string, memory.Actor) (localdolt.DocResult, error)
	Render(context.Context) (render.Result, error)
	RepoStatus(context.Context, localdolt.RepoStatusOptions) (localdolt.RepoStatusReport, error)
	ExportMemory(context.Context, localdolt.ExportMemoryOptions) (localdolt.InteropResult, error)
	ImportMemory(context.Context, localdolt.ImportMemoryOptions) (localdolt.InteropResult, error)
	FactAdd(context.Context, localdolt.FactAddOptions) (localdolt.HumanMemoryResult, error)
	FactVerify(context.Context, string, memory.Actor) (localdolt.HumanMemoryResult, error)
	FactSupersede(context.Context, string, string, memory.Actor) (localdolt.HumanMemoryResult, error)
	DecisionAdd(context.Context, localdolt.DecisionAddOptions) (localdolt.HumanMemoryResult, error)
	DecisionSetSummary(context.Context, string, string, memory.Actor) (localdolt.HumanMemoryResult, error)
	DecisionSupersede(context.Context, string, string, memory.Actor) (localdolt.HumanMemoryResult, error)
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
	Error   string         `json:"error,omitempty"`
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

// reviewAcceptResult preserves the store gate's established post-merge
// contract over IPC: cleanup may fail after Result proves main moved.
type reviewAcceptResult struct {
	Result       localdolt.AcceptResult `json:"result"`
	CleanupError string                 `json:"cleanupError,omitempty"`
}

// Transfer errors can follow confirmed promotion or leave a remote outcome
// unknown. Preserve both the result and error without resubmitting the operation.
type transferResult struct {
	Result localdolt.TransferResult `json:"result"`
	Error  string                   `json:"error,omitempty"`
}

type addRemoteResult struct {
	Result localdolt.Remote `json:"result"`
	Error  string           `json:"error,omitempty"`
}

type docIdentityArgs struct {
	Ident string       `json:"ident"`
	Actor memory.Actor `json:"actor"`
}

// Document data can commit before optional local config finalization fails.
// The authenticated reply carries both facts without replaying the mutation.
type docMutationResult struct {
	Result localdolt.DocResult `json:"result"`
	Error  string              `json:"error,omitempty"`
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
	case opHistory:
		var args localdolt.HistoryOptions
		args, err = operationArgs[localdolt.HistoryOptions](req.Args)
		if err == nil {
			result, err = h.store.History(ctx, args)
		}
	case opCapturePromotion:
		var args lastChangedArgs
		args, err = operationArgs[lastChangedArgs](req.Args)
		if err == nil {
			result, err = h.store.CapturePromotion(ctx, args.SourceType, args.SourceID)
		}
	case opCaptureRecall:
		var args store.RecallSnapshotOptions
		args, err = operationArgs[store.RecallSnapshotOptions](req.Args)
		if err == nil {
			result, err = h.store.CaptureRecall(ctx, args)
		}
	case opExportMemory, opImportMemory:
		// The owner reads/publishes the checked local bundle and performs the
		// entire import once. A populated failure result preserves durable progress.
		var completed localdolt.InteropResult
		var operationErr error
		if req.Operation == opExportMemory {
			args, decodeErr := operationArgs[localdolt.ExportMemoryOptions](req.Args)
			if decodeErr != nil {
				err = decodeErr
				break
			}
			completed, operationErr = h.store.ExportMemory(ctx, args)
		} else {
			args, decodeErr := operationArgs[localdolt.ImportMemoryOptions](req.Args)
			if decodeErr != nil {
				err = decodeErr
				break
			}
			completed, operationErr = h.store.ImportMemory(ctx, args)
		}
		if operationErr != nil {
			completed.Error = operationErr.Error()
		}
		result = completed
	case opFactAdd, opFactVerify, opFactSupersede, opDecisionAdd, opDecisionSummary, opDecisionSupersede:
		changed, changeErr := h.humanMemoryOperation(ctx, req)
		if changeErr != nil {
			changed.Error = changeErr.Error()
		}
		result = changed
	case opDocList:
		result, err = h.store.DocList(ctx)
	case opDocShow:
		args, decodeErr := operationArgs[docIdentityArgs](req.Args)
		if decodeErr != nil {
			err = decodeErr
			break
		}
		result, err = h.store.DocShow(ctx, args.Ident)
	case opDocAdd, opDocRemove:
		var changed localdolt.DocResult
		var changeErr error
		if req.Operation == opDocAdd {
			args, decodeErr := operationArgs[localdolt.DocAddOptions](req.Args)
			if decodeErr != nil {
				err = decodeErr
				break
			}
			changed, changeErr = h.store.DocAdd(ctx, args)
		} else {
			args, decodeErr := operationArgs[docIdentityArgs](req.Args)
			if decodeErr != nil {
				err = decodeErr
				break
			}
			changed, changeErr = h.store.DocRemove(ctx, args.Ident, args.Actor)
		}
		wire := docMutationResult{Result: changed}
		if changeErr != nil {
			wire.Error = changeErr.Error()
		}
		result = wire
	case opRender:
		// A complete render executes once in the owner. Its typed result also
		// preserves backups and partial replacements when preparation/finalization
		// fails; no destination or query is accepted from the IPC client.
		rendered, renderErr := h.render(ctx)
		if renderErr != nil {
			rendered.Error = renderErr.Error()
		}
		result = rendered
	case opRepoStatus:
		args, decodeErr := operationArgs[localdolt.RepoStatusOptions](req.Args)
		if decodeErr != nil {
			err = decodeErr
			break
		}
		result, err = h.store.RepoStatus(ctx, args)
	case opListRemotes:
		result, err = h.store.ListRemotes(ctx)
	case opAddRemote:
		args, decodeErr := operationArgs[localdolt.Remote](req.Args)
		if decodeErr != nil {
			err = decodeErr
			break
		}
		added, addErr := h.store.AddRemote(ctx, args)
		wire := addRemoteResult{Result: added}
		if addErr != nil {
			wire.Error = addErr.Error()
		}
		result = wire
	case opPush, opPull:
		args, decodeErr := operationArgs[localdolt.TransferOptions](req.Args)
		if decodeErr != nil {
			err = decodeErr
			break
		}
		var transferred localdolt.TransferResult
		var transferErr error
		if req.Operation == opPush {
			transferred, transferErr = h.store.Push(ctx, args)
		} else {
			transferred, transferErr = h.store.Pull(ctx, args)
		}
		wire := transferResult{Result: transferred}
		if transferErr != nil {
			wire.Error = transferErr.Error()
		}
		result = wire
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
		wire := recordCommandResult{Command: command, Commit: commit}
		if recordErr != nil && commit != "" {
			wire.Error = recordErr.Error()
		} else {
			err = recordErr
		}
		result = wire
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
		accepted, acceptErr := h.reviewAccept(ctx, args.ID, args.ExpectedCommit, args.Reviewer, args.Force)
		if acceptErr != nil && accepted.Commit == "" && accepted.Proposal.Target != localdolt.TargetGlobal {
			err = acceptErr
			break
		}
		result = reviewAcceptResult{Result: accepted}
		if acceptErr != nil {
			accepted.Unknown = errors.Is(acceptErr, store.ErrCommitUnknown)
			result = reviewAcceptResult{Result: accepted, CleanupError: acceptErr.Error()}
		}
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
	if err := localdolt.ValidateJSONUnicode(raw); err != nil {
		return args, fmt.Errorf("decode store operation arguments: %w", err)
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return args, fmt.Errorf("decode store operation arguments: %w", err)
	}
	return args, nil
}
