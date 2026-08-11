// Package storeipc carries store operations over the single-owner IPC
// endpoint (PRD §5.2.1).
//
// The endpoint in internal/ipc exists so that a CLI can find the process
// that owns the embedded store and prove it is live; until M0 it served
// only its own health check, and Config.Handler was the token-gated seam
// left for the routes that would carry real work. This package is that
// seam's first occupant: it lets a second process submit a
// version-controlled write and run a read through the owner, so the M0 rig
// can drive the topology PRD §5.2 specifies — one process holding the Dolt
// data directory, every other process routed through it — rather than
// simulating it.
//
// # Wire format
//
// Two routes, JSON in and JSON out, POST only, both behind the endpoint's
// existing X-Memdolt-Token check (internal/ipc applies it to everything
// Config.Handler serves, so nothing here re-implements authentication and
// nothing here ever sees the token):
//
//	POST /v0/store/commit  {statements, message, author} -> {hash, rowsAffected}
//	POST /v0/store/query   {sql, args}                   -> {columns, rows}
//
// It is deliberately the smallest thing that carries store.Store's M0
// subset, including Query's raw SQL and untyped result grid, because the M0
// rig has to interrogate dolt_log and its own probe tables. Before the Store
// boundary was enforced, this route forwarded every statement unchanged and
// could leave an uncommitted write in the owner's working set. It now inherits
// Store.Query's one-SELECT-or-SHOW restriction; that restriction binds this
// query route, while the commit route remains the version-controlled write
// path. M1 replaces both ends with the typed memory operations of PRD §5.1.
// Nothing outside M0 should be built against this shape.
//
// # What the result grid can carry
//
// Cells cross the wire as text or null. A value the driver hands back as
// bytes that are not valid UTF-8 is refused rather than mangled into a
// string that would compare unequal on the other side and look like
// corruption. Likewise a result set larger than MaxRows is refused, not
// truncated: a silently short answer is indistinguishable from missing
// data, which is the one thing the rig this serves exists to detect.
package storeipc

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/kninetimmy/memdolt/internal/store"
)

const (
	// CommitPath applies a write and records it as one Dolt commit.
	CommitPath = "/v0/store/commit"

	// QueryPath runs exactly one read-only SELECT or SHOW statement.
	QueryPath = "/v0/store/query"

	// DefaultMaxRows bounds a result set. Memory-scale tables (PRD §8.1
	// puts the corpus at 10³–10⁴ rows) fit inside it with room to spare.
	DefaultMaxRows = 200_000

	// DefaultMaxRequestBytes bounds a request body.
	DefaultMaxRequestBytes = 4 << 20
)

// ErrTooManyRows reports a result set larger than the configured bound.
var ErrTooManyRows = errors.New("result set is larger than the endpoint will carry")

// Actor is the wire form of store.Actor.
type Actor struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// Statement is the wire form of store.Statement. Args are bound by the
// owner, never formatted into the SQL text.
type Statement struct {
	SQL  string `json:"sql"`
	Args []any  `json:"args,omitempty"`
}

// CommitRequest is the wire form of store.CommitRequest.
//
// Text carries the memory the write records, which the owner matches
// against the repository's deny-list (PRD §11.3). It crosses the wire
// because the deny-list is enforced where the store is, and a routed
// write that dropped its text on the way would be the one write nothing
// scanned. NoText crosses it for the same reason: the owner refuses a
// request that declares neither, so the declaration has to survive the
// hop rather than be re-derived from an empty field.
type CommitRequest struct {
	Statements []Statement `json:"statements"`
	Text       []string    `json:"text,omitempty"`
	NoText     bool        `json:"noText,omitempty"`
	Message    string      `json:"message"`
	Author     Actor       `json:"author"`
}

// CommitResponse is the wire form of store.CommitResult.
type CommitResponse struct {
	Hash         string `json:"hash"`
	RowsAffected int64  `json:"rowsAffected"`
}

// QueryRequest is exactly one read-only SELECT or SHOW statement. The owner
// enforces that boundary before execution.
type QueryRequest struct {
	SQL  string `json:"sql"`
	Args []any  `json:"args,omitempty"`
}

// QueryResponse is a result grid. A nil cell is SQL NULL.
type QueryResponse struct {
	Columns []string    `json:"columns"`
	Rows    [][]*string `json:"rows"`
}

// ErrorResponse is the body of every non-200 answer. The owner's error
// text is passed through: the endpoint is loopback-only, token-gated and
// same-user, and a caller that cannot see why a write failed cannot
// classify the failure.
type ErrorResponse struct {
	Error string `json:"error"`
}

// Config configures a handler.
type Config struct {
	// Store is the store this process owns. Required.
	Store store.Store

	// MaxRows bounds a result set. Zero means DefaultMaxRows.
	MaxRows int

	// MaxRequestBytes bounds a request body. Zero means
	// DefaultMaxRequestBytes.
	MaxRequestBytes int64

	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

type handler struct {
	store           store.Store
	maxRows         int
	maxRequestBytes int64
	logger          *slog.Logger
}

// NewHandler returns the routes an owning process serves on its IPC
// endpoint. Pass it as ipc.Config.Handler; the endpoint applies the token
// check to everything it serves.
func NewHandler(cfg Config) (http.Handler, error) {
	if cfg.Store == nil {
		return nil, errors.New("storeipc: a store is required")
	}
	h := &handler{
		store:           cfg.Store,
		maxRows:         cfg.MaxRows,
		maxRequestBytes: cfg.MaxRequestBytes,
		logger:          cfg.Logger,
	}
	if h.maxRows <= 0 {
		h.maxRows = DefaultMaxRows
	}
	if h.maxRequestBytes <= 0 {
		h.maxRequestBytes = DefaultMaxRequestBytes
	}
	if h.logger == nil {
		h.logger = slog.Default()
	}

	mux := http.NewServeMux()
	mux.HandleFunc(CommitPath, h.handleCommit)
	mux.HandleFunc(QueryPath, h.handleQuery)
	return mux, nil
}

func (h *handler) handleCommit(w http.ResponseWriter, r *http.Request) {
	var req CommitRequest
	if !h.decode(w, r, &req) {
		return
	}

	statements := make([]store.Statement, 0, len(req.Statements))
	for i, stmt := range req.Statements {
		args, err := normalizeArgs(stmt.Args)
		if err != nil {
			h.fail(w, http.StatusBadRequest, fmt.Errorf("statement %d: %w", i, err))
			return
		}
		statements = append(statements, store.Statement{SQL: stmt.SQL, Args: args})
	}

	result, err := h.store.Commit(r.Context(), store.CommitRequest{
		Statements: statements,
		Text:       req.Text,
		NoText:     req.NoText,
		Message:    req.Message,
		Author:     store.Actor{Name: req.Author.Name, Email: req.Author.Email},
	})
	if err != nil {
		h.fail(w, http.StatusInternalServerError, err)
		return
	}
	h.respond(w, CommitResponse{Hash: result.Hash, RowsAffected: result.RowsAffected})
}

func (h *handler) handleQuery(w http.ResponseWriter, r *http.Request) {
	var req QueryRequest
	if !h.decode(w, r, &req) {
		return
	}

	args, err := normalizeArgs(req.Args)
	if err != nil {
		h.fail(w, http.StatusBadRequest, err)
		return
	}

	rows, err := h.store.Query(r.Context(), req.SQL, args...)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, err)
		return
	}
	defer func() { _ = rows.Close() }()

	grid, err := encodeRows(rows, h.maxRows)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, err)
		return
	}
	h.respond(w, grid)
}

// decode reads a JSON request body. It reports whether decoding succeeded;
// when it did not, it has already answered the request.
func (h *handler) decode(w http.ResponseWriter, r *http.Request, into any) bool {
	if r.Method != http.MethodPost {
		h.fail(w, http.StatusMethodNotAllowed, fmt.Errorf("%s accepts POST, not %s", r.URL.Path, r.Method))
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, h.maxRequestBytes))
	// Numbers stay exact until normalizeArgs decides what they are; a bare
	// decode into any would turn every integer into a float64 and hand the
	// SQL engine a different value than the caller sent.
	dec.UseNumber()
	if err := dec.Decode(into); err != nil {
		h.fail(w, http.StatusBadRequest, fmt.Errorf("decode request body: %w", err))
		return false
	}
	return true
}

func (h *handler) respond(w http.ResponseWriter, body any) {
	// Encoded before anything is written, so that a failure part-way
	// through cannot leave a truncated body behind a 200.
	encoded, err := json.Marshal(body)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, fmt.Errorf("encode response: %w", err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(encoded); err != nil {
		h.logger.Warn("memdolt: storeipc response write failed", "error", err.Error())
	}
}

func (h *handler) fail(w http.ResponseWriter, code int, err error) {
	encoded, marshalErr := json.Marshal(ErrorResponse{Error: err.Error()})
	if marshalErr != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if _, writeErr := w.Write(encoded); writeErr != nil {
		h.logger.Warn("memdolt: storeipc error response write failed", "error", writeErr.Error())
	}
}

// normalizeArgs converts JSON-decoded bound parameters into the Go types
// database/sql accepts. It refuses anything it cannot convert rather than
// passing a value the engine would interpret differently from what the
// caller sent.
func normalizeArgs(args []any) ([]any, error) {
	if len(args) == 0 {
		return nil, nil
	}
	out := make([]any, len(args))
	for i, arg := range args {
		switch v := arg.(type) {
		case nil:
			out[i] = nil
		case string, bool:
			out[i] = v
		case json.Number:
			if n, err := v.Int64(); err == nil {
				out[i] = n
				continue
			}
			f, err := v.Float64()
			if err != nil {
				return nil, fmt.Errorf("argument %d: %q is not a number database/sql can bind: %w", i, v.String(), err)
			}
			out[i] = f
		default:
			return nil, fmt.Errorf("argument %d has type %T, which this endpoint does not carry", i, arg)
		}
	}
	return out, nil
}

// encodeRows renders a result set as text cells. It fails rather than
// truncate or mangle; see the package documentation.
func encodeRows(rows *sql.Rows, maxRows int) (QueryResponse, error) {
	columns, err := rows.Columns()
	if err != nil {
		return QueryResponse{}, fmt.Errorf("read result columns: %w", err)
	}

	grid := QueryResponse{Columns: columns, Rows: [][]*string{}}
	cells := make([]any, len(columns))
	pointers := make([]any, len(columns))
	for i := range cells {
		pointers[i] = &cells[i]
	}

	for rows.Next() {
		if len(grid.Rows) >= maxRows {
			return QueryResponse{}, fmt.Errorf("%w (limit %d)", ErrTooManyRows, maxRows)
		}
		if err := rows.Scan(pointers...); err != nil {
			return QueryResponse{}, fmt.Errorf("scan result row %d: %w", len(grid.Rows), err)
		}
		row := make([]*string, len(columns))
		for i, cell := range cells {
			text, err := encodeCell(cell)
			if err != nil {
				return QueryResponse{}, fmt.Errorf("result row %d, column %q: %w", len(grid.Rows), columns[i], err)
			}
			row[i] = text
		}
		grid.Rows = append(grid.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return QueryResponse{}, fmt.Errorf("read result rows: %w", err)
	}
	return grid, nil
}

func encodeCell(cell any) (*string, error) {
	switch v := cell.(type) {
	case nil:
		return nil, nil
	case string:
		return &v, nil
	case []byte:
		if !utf8.Valid(v) {
			return nil, errors.New("value is bytes that are not valid UTF-8, which this endpoint does not carry")
		}
		text := string(v)
		return &text, nil
	case bool:
		text := strconv.FormatBool(v)
		return &text, nil
	case int64:
		text := strconv.FormatInt(v, 10)
		return &text, nil
	case float64:
		text := strconv.FormatFloat(v, 'g', -1, 64)
		return &text, nil
	case time.Time:
		text := v.UTC().Format(time.RFC3339Nano)
		return &text, nil
	default:
		return nil, fmt.Errorf("value has type %T, which this endpoint does not carry", cell)
	}
}
