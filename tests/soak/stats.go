//go:build soak

package soak

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kninetimmy/memdolt/internal/ipc"
	"github.com/kninetimmy/memdolt/internal/store"
)

const (
	// errorSamplesPerClass bounds how many distinct messages are kept per
	// class. Classes are a summary; the samples are what let a reader of
	// the write-up see the actual text behind one, including a class the
	// rig could not name.
	errorSamplesPerClass = 5

	// errorSampleBytes truncates a kept message.
	errorSampleBytes = 400

	// missingKeySamples bounds how many keys a reader could not see are
	// named in the summary.
	missingKeySamples = 20

	// statsSnapshotInterval is how often a role rewrites its stats file. A
	// role that is killed loses at most this much of its own diagnostics;
	// nothing the §16 gate is computed from lives here, because that comes
	// from the ledger and the store.
	statsSnapshotInterval = 500 * time.Millisecond
)

// ErrorTally groups failures by class and keeps a bounded sample of the
// text behind each one.
type ErrorTally struct {
	Counts  map[string]int      `json:"counts"`
	Samples map[string][]string `json:"samples"`
}

func newErrorTally() *ErrorTally {
	return &ErrorTally{Counts: map[string]int{}, Samples: map[string][]string{}}
}

func (t *ErrorTally) add(class, text string) {
	t.Counts[class]++
	if len(text) > errorSampleBytes {
		text = text[:errorSampleBytes] + "…"
	}
	samples := t.Samples[class]
	if len(samples) >= errorSamplesPerClass {
		return
	}
	for _, existing := range samples {
		if existing == text {
			return
		}
	}
	t.Samples[class] = append(samples, text)
}

func (t *ErrorTally) merge(other *ErrorTally) {
	if other == nil {
		return
	}
	for class, count := range other.Counts {
		t.Counts[class] += count
	}
	for class, samples := range other.Samples {
		for _, sample := range samples {
			existing := t.Samples[class]
			if len(existing) >= errorSamplesPerClass {
				break
			}
			duplicate := false
			for _, e := range existing {
				if e == sample {
					duplicate = true
					break
				}
			}
			if !duplicate {
				t.Samples[class] = append(existing, sample)
			}
		}
	}
}

func (t *ErrorTally) total() int {
	sum := 0
	for _, count := range t.Counts {
		sum += count
	}
	return sum
}

func (t *ErrorTally) classes() []string {
	names := make([]string, 0, len(t.Counts))
	for class := range t.Counts {
		names = append(names, class)
	}
	sort.Strings(names)
	return names
}

// RoleStats is one process's own account of what it did. It is diagnostic:
// the verdict is computed from the ledger and the store, never from here.
type RoleStats struct {
	Role     string `json:"role"`
	ID       string `json:"id"`
	PID      int    `json:"pid"`
	CleanEnd bool   `json:"cleanEnd"`

	WritesAttempted     int `json:"writesAttempted"`
	WritesCommitted     int `json:"writesCommitted"`
	WritesRefused       int `json:"writesRefused"`
	WritesIndeterminate int `json:"writesIndeterminate"`

	ReadsAttempted           int      `json:"readsAttempted"`
	ReadsFailed              int      `json:"readsFailed"`
	ReadsMissingCommittedRow int      `json:"readsMissingCommittedRow"`
	ReadsMismatchedPayload   int      `json:"readsMismatchedPayload"`
	RowCountRegressions      int      `json:"rowCountRegressions"`
	MissingKeySamples        []string `json:"missingKeySamples,omitempty"`

	Errors *ErrorTally `json:"errors"`

	UpdatedAtUnixNS int64 `json:"updatedAtUnixNs"`

	mu   sync.Mutex
	path string
}

func newRoleStats(role, id, path string) *RoleStats {
	return &RoleStats{Role: role, ID: id, PID: os.Getpid(), Errors: newErrorTally(), path: path}
}

func (s *RoleStats) recordWrite(outcome Outcome, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.WritesAttempted++
	switch outcome {
	case OutcomeCommitted:
		s.WritesCommitted++
	case OutcomeRefused:
		s.WritesRefused++
	case OutcomeIndeterminate:
		s.WritesIndeterminate++
	}
	if err != nil {
		s.Errors.add(classify(err), err.Error())
	}
}

func (s *RoleStats) recordRead(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ReadsAttempted++
	if err != nil {
		s.ReadsFailed++
		s.Errors.add(classify(err), err.Error())
	}
}

func (s *RoleStats) recordMissingRow(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ReadsMissingCommittedRow++
	if len(s.MissingKeySamples) < missingKeySamples {
		s.MissingKeySamples = append(s.MissingKeySamples, key)
	}
}

func (s *RoleStats) recordMismatch(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ReadsMismatchedPayload++
	if len(s.MissingKeySamples) < missingKeySamples {
		s.MissingKeySamples = append(s.MissingKeySamples, key+" (payload mismatch)")
	}
}

func (s *RoleStats) recordCountRegression(from, to int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.RowCountRegressions++
	s.Errors.add("row_count_regression", fmt.Sprintf("row count fell from %d to %d", from, to))
}

func (s *RoleStats) markCleanEnd() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.CleanEnd = true
}

// write publishes the current counters atomically, so that a reader never
// sees a half-written file even if the role is killed mid-snapshot.
func (s *RoleStats) write() error {
	s.mu.Lock()
	s.UpdatedAtUnixNS = time.Now().UnixNano()
	encoded, err := json.MarshalIndent(s, "", "  ")
	s.mu.Unlock()
	if err != nil {
		return fmt.Errorf("encode stats: %w", err)
	}

	temp := s.path + ".tmp"
	if err := os.WriteFile(temp, append(encoded, '\n'), 0o600); err != nil {
		return fmt.Errorf("write stats %s: %w", temp, err)
	}
	if err := os.Rename(temp, s.path); err != nil {
		return fmt.Errorf("publish stats %s: %w", s.path, err)
	}
	return nil
}

// snapshotLoop keeps the stats file roughly current while the role runs.
func (s *RoleStats) snapshotLoop(ctx context.Context) {
	ticker := time.NewTicker(statsSnapshotInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.write(); err != nil {
				fmt.Fprintln(os.Stderr, "soak: stats snapshot:", err)
			}
		}
	}
}

// readRoleStats reads every published stats file in dir.
func readRoleStats(dir string) ([]*RoleStats, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read stats directory %s: %w", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)

	stats := make([]*RoleStats, 0, len(names))
	for _, name := range names {
		path := filepath.Join(dir, name)
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read stats %s: %w", path, err)
		}
		var parsed RoleStats
		if err := json.Unmarshal(raw, &parsed); err != nil {
			return nil, fmt.Errorf("decode stats %s: %w", path, err)
		}
		if parsed.Errors == nil {
			parsed.Errors = newErrorTally()
		}
		stats = append(stats, &parsed)
	}
	return stats, nil
}

// classify names the kind of a failure so that failures can be counted by
// class rather than by message.
//
// Nothing it does not recognise is folded into a neighbouring class: an
// unrecognised failure is "unclassified" and keeps its text, because a
// spike whose job is to find out what goes wrong must not quietly rename
// what it did not expect.
func classify(err error) string {
	if err == nil {
		return ""
	}

	switch {
	case errors.Is(err, store.ErrLocked):
		return "store_locked"
	case errors.Is(err, store.ErrNotOpen):
		return "store_not_open"
	case errors.Is(err, context.DeadlineExceeded):
		return "context_deadline_exceeded"
	case errors.Is(err, context.Canceled):
		return "context_canceled"
	}

	// The owner answered: classify what it said, and keep the fact that it
	// was the owner speaking rather than the wire.
	var status *ipc.StatusError
	if errors.As(err, &status) {
		return fmt.Sprintf("owner_%d_%s", status.Code, classifyText(status.Body))
	}

	// No answer: name the transport failure as precisely as the standard
	// library allows.
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "transport_timeout"
	}
	if class := classifyTransport(err.Error()); class != "" {
		return class
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return "transport_" + opErr.Op
	}
	return classifyText(err.Error())
}

func classifyTransport(text string) string {
	lower := strings.ToLower(text)
	switch {
	case strings.Contains(lower, "actively refused"), strings.Contains(lower, "connection refused"):
		return "transport_connection_refused"
	case strings.Contains(lower, "connection reset"), strings.Contains(lower, "forcibly closed"):
		return "transport_connection_reset"
	case strings.Contains(lower, "only one usage of each socket address"),
		strings.Contains(lower, "address already in use"):
		// Ephemeral-port exhaustion: the caller is opening a new connection
		// per request instead of reusing one. Named explicitly because the
		// first long run of this rig produced a quarter of a million of them
		// and they must not read as "the store was busy".
		return "transport_ephemeral_port_exhaustion"
	case strings.Contains(lower, "eof"):
		return "transport_eof"
	default:
		return ""
	}
}

func classifyText(text string) string {
	lower := strings.ToLower(text)
	switch {
	case strings.Contains(lower, "database is read only"):
		return "dolt_read_only"
	case strings.Contains(lower, "nothing to commit"):
		return "dolt_nothing_to_commit"
	case strings.Contains(lower, "conflict"):
		return "dolt_conflict"
	case strings.Contains(lower, "duplicate primary key"), strings.Contains(lower, "duplicate entry"):
		return "sql_duplicate_key"
	case strings.Contains(lower, "deadlock"):
		return "sql_deadlock"
	case strings.Contains(lower, "table not found"), strings.Contains(lower, "doesn't exist"):
		return "sql_table_not_found"
	case strings.Contains(lower, "database is locked"):
		return "dolt_database_locked"
	case strings.Contains(lower, "is locked"):
		return "lock_held"
	default:
		return "unclassified"
	}
}
