package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kninetimmy/memdolt/internal/layout"
	"github.com/kninetimmy/memdolt/internal/memory"
	"github.com/kninetimmy/memdolt/internal/retrieval"
	"github.com/kninetimmy/memdolt/internal/search"
	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
	"github.com/kninetimmy/memdolt/internal/storeipc"
)

const (
	noteBatchInterval = 5 * time.Minute
	noteFlushTimeout  = time.Minute
	defaultListLimit  = 25
)

// Backend is the already-open owner store the MCP application uses. It is the
// same typed and store.Store surface the CLI reaches directly or over IPC,
// plus the complete application review gate; a raw storage merge cannot be
// registered as review_pending.
type Backend interface {
	storeipc.Backend
	DataDir() string
	ReviewAcceptExpected(context.Context, string, string, store.Actor, bool) (localdolt.AcceptResult, error)
}

// Toolset owns the implemented tools and their session-scoped state: notes waiting
// for the five-minute or orderly-shutdown flush and single-use elicitation
// rows waiting for one short-lived response.
type Toolset struct {
	store    Backend
	baseDir  string
	interval time.Duration

	mu       sync.Mutex
	groups   []noteGroup
	timer    *time.Timer
	flushErr error
	closed   bool

	elicit    *elicitationStateStore
	elicitErr error
}

type noteGroup struct {
	actor memory.Actor
	notes []memory.Note
}

// RegisterTools adds exactly the implemented surface. Later-milestone
// names are intentionally absent rather than registered as refusing stubs.
func RegisterTools(server *mcp.Server, baseDir string, st Backend) *Toolset {
	return registerTools(server, baseDir, st, noteBatchInterval)
}

func registerTools(server *mcp.Server, baseDir string, st Backend, interval time.Duration) *Toolset {
	elicit, elicitErr := newElicitationStateStore()
	tools := &Toolset{
		store: st, baseDir: baseDir, interval: interval,
		elicit: elicit, elicitErr: elicitErr,
	}
	mcp.AddTool(server, &mcp.Tool{Name: "status", Description: "Return the current schema and committed-memory counts."}, tools.status)
	mcp.AddTool(server, &mcp.Tool{Name: "repo_status", Description: "Inspect committed local and remote main (default origin) without promotion. local requests offline status; diff includes exact local-to-remote committed row values. Fetch data and the selected tracking ref may remain. Dirty divergence needs clean main for merge assessment."}, tools.repoStatus)
	mcp.AddTool(server, &mcp.Tool{Name: "repo_pull", Description: "Pull committed remote main; compatible divergence merges as the requesting agent. Actual conflicts require complete human form choices bound to both heads, committed together as user. Nine modern forms return a reachable continuation; legacy clients receive one complete form. Without form support use the CLI remedy."}, tools.repoPull)
	mcp.AddTool(server, &mcp.Tool{Name: "repo_push", Description: "Publish captured committed main to one configured remote by fast-forward; never force or publish proposals. Password comes only from the executing owner's environment. An uncertain reply requires inspection before retrying."}, tools.repoPush)
	mcp.AddTool(server, &mcp.Tool{Name: "recall", Description: "Recall ranked committed facts, decisions, tasks, and document chunks."}, tools.recall)
	mcp.AddTool(server, &mcp.Tool{Name: "search", Description: "Search committed decision titles and rationales."}, tools.search)
	mcp.AddTool(server, &mcp.Tool{Name: "list_tasks", Description: "List committed tasks by status, oldest first."}, tools.listTasks)
	mcp.AddTool(server, &mcp.Tool{Name: "list_decisions", Description: "List committed decisions, newest first."}, tools.listDecisions)
	mcp.AddTool(server, &mcp.Tool{Name: "list_facts", Description: "List committed facts by key, optionally under one literal dotted-key prefix; superseded rows remain visible."}, tools.listFacts)
	mcp.AddTool(server, &mcp.Tool{Name: "list_proposals", Description: "List single-commit proposal branches waiting for review, oldest first."}, tools.listProposals)
	mcp.AddTool(server, &mcp.Tool{Name: "get_command", Description: "Return the recorded command for one kind."}, tools.getCommand)
	mcp.AddTool(server, &mcp.Tool{Name: "task_add", Description: "Create a directly committed task."}, tools.taskAdd)
	mcp.AddTool(server, &mcp.Tool{Name: "task_done", Description: "Mark a task done in a directly attributed commit."}, tools.taskDone)
	mcp.AddTool(server, &mcp.Tool{Name: "log_session_note", Description: "Queue a session note for the five-minute or orderly-shutdown batch commit."}, tools.logSessionNote)
	mcp.AddTool(server, &mcp.Tool{Name: "record_command", Description: "Record a command outcome in a directly attributed commit."}, tools.recordCommand)
	mcp.AddTool(server, &mcp.Tool{Name: "propose_fact", Description: "Stage a fact on a single-commit proposal branch without moving main."}, tools.proposeFact)
	mcp.AddTool(server, &mcp.Tool{Name: "propose_decision", Description: "Stage a decision on a single-commit proposal branch without moving main."}, tools.proposeDecision)
	mcp.AddTool(server, &mcp.Tool{Name: "propose_supersede", Description: "Stage a fact supersession and replacement on a single-commit proposal branch without moving main."}, tools.proposeSupersede)
	mcp.AddTool(server, &mcp.Tool{Name: "review_pending", Description: "Offer repository proposals for human elicitation review; global proposals remain CLI-only."}, tools.reviewPending)
	mcp.AddTool(server, &mcp.Tool{Name: "doc_add", Description: "Ingest a confined repository Markdown document in one attributed commit; the first document enables default recall. Unchanged bytes are a no-op."}, tools.docAdd)
	mcp.AddTool(server, &mcp.Tool{Name: "render", Description: "Render committed main to configured local PROJECT.md and PROJECT_LEDGER.md; report backups and partial failures. Pending notes and proposals are excluded."}, tools.render)
	return tools
}

type emptyInput struct{}

type statusOutput struct {
	DataDir       string `json:"dataDir"`
	SchemaVersion int    `json:"schemaVersion"`
	Facts         int    `json:"facts"`
	Decisions     int    `json:"decisions"`
	TasksOpen     int    `json:"tasksOpen"`
	TasksTotal    int    `json:"tasksTotal"`
	Commands      int    `json:"commands"`
	Proposals     int    `json:"proposals"`
}

func (t *Toolset) status(ctx context.Context, _ *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, statusOutput, error) {
	version, err := t.store.SchemaVersion(ctx)
	if err != nil {
		return nil, statusOutput{}, err
	}
	queries := []string{
		"SELECT COUNT(*) FROM facts AS OF 'main'",
		"SELECT COUNT(*) FROM decisions AS OF 'main'",
		"SELECT COUNT(*) FROM tasks AS OF 'main' WHERE status = 'open'",
		"SELECT COUNT(*) FROM tasks AS OF 'main'",
		"SELECT COUNT(*) FROM commands AS OF 'main'",
	}
	counts := make([]int, len(queries))
	for i, query := range queries {
		counts[i], err = queryCount(ctx, t.store, query)
		if err != nil {
			return nil, statusOutput{}, err
		}
	}
	proposals, err := t.store.PendingProposals(ctx)
	if err != nil {
		return nil, statusOutput{}, err
	}
	return &mcp.CallToolResult{}, statusOutput{
		DataDir: t.store.DataDir(), SchemaVersion: version, Facts: counts[0], Decisions: counts[1],
		TasksOpen: counts[2], TasksTotal: counts[3], Commands: counts[4], Proposals: len(proposals),
	}, nil
}

type recallInput struct {
	Query          string   `json:"query" jsonschema:"the text to recall"`
	Mode           string   `json:"mode,omitempty" jsonschema:"optional retrieval mode: fts or hybrid"`
	MaxResults     int      `json:"max_results,omitempty" jsonschema:"maximum returned results; zero uses repository config"`
	SourceTypes    []string `json:"source_types,omitempty" jsonschema:"optional fact, decision, task, or doc_chunk filters"`
	AcceptedOnly   *bool    `json:"accepted_only,omitempty"`
	IncludeStale   *bool    `json:"include_stale,omitempty"`
	NoRerank       bool     `json:"no_rerank,omitempty"`
	MinRerankScore *float32 `json:"min_rerank_score,omitempty"`
	Provenance     bool     `json:"provenance,omitempty" jsonschema:"include last-changing Dolt commits"`
}

func (t *Toolset) recall(ctx context.Context, _ *mcp.CallToolRequest, in recallInput) (*mcp.CallToolResult, retrieval.Response, error) {
	options := retrieval.Options{
		Query: in.Query, MaxResults: in.MaxResults, SourceTypes: in.SourceTypes,
		AcceptedOnly: in.AcceptedOnly, IncludeStale: in.IncludeStale,
		MinRerankScore: in.MinRerankScore, Provenance: in.Provenance,
	}
	if in.Mode != "" {
		mode, err := retrieval.ParseMode(in.Mode)
		if err != nil {
			return nil, retrieval.Response{}, err
		}
		options.Mode = mode
	}
	if in.NoRerank {
		use := false
		options.UseReranker = &use
	}
	response, err := retrieval.Run(ctx, t.store, t.baseDir, options)
	return &mcp.CallToolResult{}, response, err
}

type searchInput struct {
	Query string `json:"query"`
	Limit int    `json:"limit,omitempty"`
}

func (t *Toolset) search(ctx context.Context, _ *mcp.CallToolRequest, in searchInput) (*mcp.CallToolResult, search.Response, error) {
	limit := in.Limit
	if limit == 0 {
		limit = search.DefaultLimit
	}
	query, err := search.Parse(in.Query, limit)
	if err != nil {
		return nil, search.Response{}, err
	}
	response, err := search.Run(ctx, t.store, query)
	return &mcp.CallToolResult{}, response, err
}

type listTasksInput struct {
	Status string `json:"status,omitempty" jsonschema:"open, done, blocked, or all; defaults to open"`
	Limit  int    `json:"limit,omitempty" jsonschema:"maximum rows; defaults to 25"`
}

type listTasksOutput struct {
	Status string        `json:"status"`
	Tasks  []memory.Task `json:"tasks"`
}

func (t *Toolset) listTasks(ctx context.Context, _ *mcp.CallToolRequest, in listTasksInput) (*mcp.CallToolResult, listTasksOutput, error) {
	status := in.Status
	if status == "" {
		status = memory.StatusOpen
	}
	limit, err := listLimit(in.Limit, defaultListLimit)
	if err != nil {
		return nil, listTasksOutput{}, err
	}
	tasks, err := memory.New(t.store, unknownActor).Tasks(ctx, status)
	if err != nil {
		return nil, listTasksOutput{}, err
	}
	if len(tasks) > limit {
		tasks = tasks[:limit]
	}
	if tasks == nil {
		tasks = []memory.Task{}
	}
	return &mcp.CallToolResult{}, listTasksOutput{Status: status, Tasks: tasks}, nil
}

type listDecisionsInput struct {
	Status string `json:"status,omitempty" jsonschema:"active, superseded, draft, or all; defaults to active"`
	Limit  int    `json:"limit,omitempty"`
}

type listDecisionsOutput struct {
	Status    string                  `json:"status"`
	Decisions []memory.DecisionRecord `json:"decisions"`
}

func (t *Toolset) listDecisions(ctx context.Context, _ *mcp.CallToolRequest, in listDecisionsInput) (*mcp.CallToolResult, listDecisionsOutput, error) {
	status := strings.TrimSpace(in.Status)
	if status == "" {
		status = "active"
	}
	limit, err := listLimit(in.Limit, search.DefaultLimit)
	if err != nil {
		return nil, listDecisionsOutput{}, err
	}
	decisions, err := memory.ListDecisions(ctx, t.store, status, limit)
	if err != nil {
		return nil, listDecisionsOutput{}, err
	}
	return &mcp.CallToolResult{}, listDecisionsOutput{Status: status, Decisions: decisions}, nil
}

type listFactsInput struct {
	Prefix string `json:"prefix,omitempty" jsonschema:"literal dotted-key prefix such as build.; percent, underscore, and backslash are ordinary key characters"`
	Limit  int    `json:"limit,omitempty"`
}

type listFactsOutput struct {
	Prefix string              `json:"prefix,omitempty"`
	Facts  []memory.FactRecord `json:"facts"`
}

func (t *Toolset) listFacts(ctx context.Context, _ *mcp.CallToolRequest, in listFactsInput) (*mcp.CallToolResult, listFactsOutput, error) {
	prefix := strings.TrimSpace(in.Prefix)
	paths, err := layout.New(t.baseDir)
	if err != nil {
		return nil, listFactsOutput{}, err
	}
	cfg, err := retrieval.LoadConfig(paths.ConfigFile())
	if err != nil {
		return nil, listFactsOutput{}, err
	}
	facts, err := memory.ListFacts(ctx, t.store, prefix, in.Limit, cfg.FactStaleAfterDays)
	if err != nil {
		return nil, listFactsOutput{}, err
	}
	return &mcp.CallToolResult{}, listFactsOutput{Prefix: prefix, Facts: facts}, nil
}

type listProposalsInput struct {
	Target string `json:"target,omitempty" jsonschema:"repo, global, or all; defaults to all"`
	Limit  int    `json:"limit,omitempty"`
}

type listProposalsOutput struct {
	Target    string                      `json:"target"`
	Proposals []localdolt.PendingProposal `json:"proposals"`
}

func (t *Toolset) listProposals(ctx context.Context, _ *mcp.CallToolRequest, in listProposalsInput) (*mcp.CallToolResult, listProposalsOutput, error) {
	target := strings.TrimSpace(in.Target)
	if target == "" {
		target = "all"
	}
	if target != "all" && target != string(localdolt.TargetRepo) && target != string(localdolt.TargetGlobal) {
		return nil, listProposalsOutput{}, fmt.Errorf("unknown proposal target %q, want repo, global, or all", target)
	}
	limit, err := listLimit(in.Limit, defaultListLimit)
	if err != nil {
		return nil, listProposalsOutput{}, err
	}
	proposals, err := t.store.PendingProposals(ctx)
	if err != nil {
		return nil, listProposalsOutput{}, err
	}
	filtered := make([]localdolt.PendingProposal, 0, len(proposals))
	for _, proposal := range proposals {
		if target == "all" || string(proposal.Target) == target {
			filtered = append(filtered, proposal)
			if len(filtered) == limit {
				break
			}
		}
	}
	return &mcp.CallToolResult{}, listProposalsOutput{Target: target, Proposals: filtered}, nil
}

type commandInput struct {
	Kind string `json:"kind"`
}

type commandOutput struct {
	Command memory.Command `json:"command"`
}

func (t *Toolset) getCommand(ctx context.Context, _ *mcp.CallToolRequest, in commandInput) (*mcp.CallToolResult, commandOutput, error) {
	command, err := memory.New(t.store, unknownActor).Command(ctx, in.Kind)
	return &mcp.CallToolResult{}, commandOutput{Command: command}, err
}

type taskAddInput struct {
	Title string `json:"title"`
	Notes string `json:"notes,omitempty"`
}

type taskWriteOutput struct {
	Task   memory.Task  `json:"task"`
	Commit string       `json:"commit"`
	Actor  memory.Actor `json:"actor"`
}

func (t *Toolset) taskAdd(ctx context.Context, _ *mcp.CallToolRequest, in taskAddInput) (*mcp.CallToolResult, taskWriteOutput, error) {
	actor := ActorFromContext(ctx)
	task, commit, err := memory.New(t.store, actor).AddTask(ctx, in.Title, in.Notes)
	return &mcp.CallToolResult{}, taskWriteOutput{Task: task, Commit: commit, Actor: actor}, err
}

type taskDoneInput struct {
	ID string `json:"id"`
}

func (t *Toolset) taskDone(ctx context.Context, _ *mcp.CallToolRequest, in taskDoneInput) (*mcp.CallToolResult, taskWriteOutput, error) {
	actor := ActorFromContext(ctx)
	task, commit, err := memory.New(t.store, actor).CompleteTask(ctx, in.ID)
	return &mcp.CallToolResult{}, taskWriteOutput{Task: task, Commit: commit, Actor: actor}, err
}

type noteInput struct {
	Text string `json:"text"`
}

type noteOutput struct {
	Note   memory.Note `json:"note"`
	Queued bool        `json:"queued"`
}

func (t *Toolset) logSessionNote(ctx context.Context, _ *mcp.CallToolRequest, in noteInput) (*mcp.CallToolResult, noteOutput, error) {
	note, err := t.queueNote(ctx, ActorFromContext(ctx), in.Text)
	return &mcp.CallToolResult{}, noteOutput{Note: note, Queued: err == nil}, err
}

type recordCommandInput struct {
	Kind     string `json:"kind"`
	Cmdline  string `json:"cmdline"`
	ExitCode int    `json:"exit_code"`
}

type commandWriteOutput struct {
	Command memory.Command `json:"command"`
	Commit  string         `json:"commit"`
	Actor   memory.Actor   `json:"actor"`
}

func (t *Toolset) recordCommand(ctx context.Context, _ *mcp.CallToolRequest, in recordCommandInput) (*mcp.CallToolResult, commandWriteOutput, error) {
	actor := ActorFromContext(ctx)
	command, commit, err := memory.New(t.store, actor).RecordCommand(ctx, in.Kind, in.Cmdline, in.ExitCode)
	return &mcp.CallToolResult{}, commandWriteOutput{Command: command, Commit: commit, Actor: actor}, err
}

type proposeFactInput struct {
	Key       string `json:"key"`
	Value     string `json:"value"`
	Rationale string `json:"rationale"`
	Kind      string `json:"kind,omitempty"`
	Evidence  string `json:"evidence,omitempty"`
	Global    bool   `json:"global,omitempty"`
}

type stagedProposalOutput struct {
	ID     string                 `json:"id"`
	Branch string                 `json:"branch"`
	Kind   localdolt.ProposalKind `json:"kind"`
	RowID  string                 `json:"rowId"`
	Commit string                 `json:"commit"`
	Actor  memory.Actor           `json:"actor"`
}

type proposeFactOutput struct {
	ID         string                 `json:"id"`
	Branch     string                 `json:"branch"`
	Kind       localdolt.ProposalKind `json:"kind"`
	RowID      string                 `json:"rowId"`
	Commit     string                 `json:"commit"`
	Actor      memory.Actor           `json:"actor"`
	Resolution string                 `json:"resolution,omitempty"`
	Canceled   bool                   `json:"canceled,omitempty"`
}

func (t *Toolset) proposeFact(ctx context.Context, req *mcp.CallToolRequest, in proposeFactInput) (*mcp.CallToolResult, proposeFactOutput, error) {
	return t.proposeFactWithConflict(ctx, req, in)
}

type proposeDecisionInput struct {
	Title                string `json:"title"`
	Rationale            string `json:"rationale"`
	Summary              string `json:"summary,omitempty"`
	AlternativesRejected string `json:"alternatives_rejected,omitempty"`
	Evidence             string `json:"evidence,omitempty"`
	Global               bool   `json:"global,omitempty"`
}

func (t *Toolset) proposeDecision(ctx context.Context, _ *mcp.CallToolRequest, in proposeDecisionInput) (*mcp.CallToolResult, stagedProposalOutput, error) {
	actor := ActorFromContext(ctx)
	staged, err := t.store.ProposeDecision(ctx, proposal(actor, in.Rationale, in.Global), localdolt.Decision{
		Title: in.Title, Rationale: in.Rationale, Summary: in.Summary,
		AlternativesRejected: in.AlternativesRejected, Evidence: in.Evidence,
	})
	return &mcp.CallToolResult{}, stagedOutput(staged, actor), err
}

type proposeSupersedeInput struct {
	SupersededID string `json:"superseded_id"`
	Key          string `json:"key"`
	Value        string `json:"value"`
	Rationale    string `json:"rationale"`
	Kind         string `json:"kind,omitempty"`
	Evidence     string `json:"evidence,omitempty"`
}

func (t *Toolset) proposeSupersede(ctx context.Context, _ *mcp.CallToolRequest, in proposeSupersedeInput) (*mcp.CallToolResult, stagedProposalOutput, error) {
	actor := ActorFromContext(ctx)
	staged, err := t.store.ProposeSupersede(ctx, proposal(actor, in.Rationale, false), in.SupersededID, localdolt.Fact{
		Key: in.Key, Value: in.Value, Kind: in.Kind, Evidence: in.Evidence,
	})
	return &mcp.CallToolResult{}, stagedOutput(staged, actor), err
}

func proposal(actor memory.Actor, rationale string, global bool) localdolt.Proposal {
	target := localdolt.TargetRepo
	if global {
		target = localdolt.TargetGlobal
	}
	return localdolt.Proposal{Rationale: rationale, Actor: actor.CommitAuthor(), Target: target}
}

func stagedOutput(staged localdolt.StagedProposal, actor memory.Actor) stagedProposalOutput {
	return stagedProposalOutput{
		ID: staged.ID, Branch: staged.Branch, Kind: staged.Kind,
		RowID: staged.RowID, Commit: staged.Commit, Actor: actor,
	}
}

func (t *Toolset) queueNote(ctx context.Context, actor memory.Actor, body string) (memory.Note, error) {
	lanes := memory.New(t.store, actor)
	note, err := lanes.PrepareNote(body)
	if err != nil {
		return memory.Note{}, err
	}
	if err := t.store.CheckWriteText(ctx, []string{note.Text, note.ActorRaw}); err != nil {
		return memory.Note{}, err
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return memory.Note{}, errors.New("mcpserver: session-note batch is closed")
	}
	if t.flushErr != nil {
		return memory.Note{}, fmt.Errorf("mcpserver: the previous session-note batch failed: %w", t.flushErr)
	}
	for i := range t.groups {
		if t.groups[i].actor == actor {
			t.groups[i].notes = append(t.groups[i].notes, note)
			if t.timer == nil {
				t.startTimer()
			}
			return note, nil
		}
	}
	t.groups = append(t.groups, noteGroup{actor: actor, notes: []memory.Note{note}})
	if t.timer == nil {
		t.startTimer()
	}
	return note, nil
}

func (t *Toolset) startTimer() {
	t.timer = time.AfterFunc(t.interval, func() {
		ctx, cancel := context.WithTimeout(context.Background(), noteFlushTimeout)
		defer cancel()
		t.mu.Lock()
		defer t.mu.Unlock()
		t.timer = nil
		if err := t.flushLocked(ctx); err != nil {
			t.flushErr = err
		}
	})
}

func (t *Toolset) flushLocked(ctx context.Context) error {
	failed := make([]noteGroup, 0, len(t.groups))
	var errs []error
	for _, group := range t.groups {
		if _, err := memory.New(t.store, group.actor).CommitNotes(ctx, group.notes); err != nil {
			failed = append(failed, group)
			errs = append(errs, fmt.Errorf("flush session-note batch for %s: %w", group.actor.Name, err))
		}
	}
	t.groups = failed
	return errors.Join(errs...)
}

// Close retries groups retained by a failed deadline flush before the owner
// store closes. Successful groups were already removed and are not recommitted.
// After this final attempt, failed groups are discarded with the returned error;
// a prior deadline error remains visible even if its shutdown retry succeeds.
func (t *Toolset) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return t.flushErr
	}
	t.closed = true
	if t.timer != nil {
		t.timer.Stop()
		t.timer = nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), noteFlushTimeout)
	defer cancel()
	t.flushErr = errors.Join(t.flushErr, t.flushLocked(ctx), t.closeElicitationState())
	t.groups = nil
	return t.flushErr
}

func queryCount(ctx context.Context, st store.Store, query string) (count int, err error) {
	rows, err := st.Query(ctx, query)
	if err != nil {
		return 0, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return 0, err
		}
		return 0, errors.New("count query returned no row")
	}
	if err := rows.Scan(&count); err != nil {
		return 0, err
	}
	return count, rows.Err()
}

func listLimit(value, fallback int) (int, error) {
	if value == 0 {
		return fallback, nil
	}
	if value < 1 {
		return 0, errors.New("list limit must be greater than zero")
	}
	return value, nil
}
