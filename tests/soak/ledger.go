//go:build soak

package soak

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// The ledger is the rig's record of what each writer meant to do, kept
// outside the store it is measuring.
//
// It has to be independent, because the question rig 1 exists to answer is
// whether the store can lose a write it acknowledged. Asking the store what
// it holds and comparing that against what the store says it holds answers
// nothing. So every writer appends its intent to its own append-only file
// and fsyncs it before it asks the store to do anything, then appends the
// store's answer afterwards. Reconciliation then compares two records with
// no common failure mode: a file written by the writer, and rows read back
// from a store re-opened in a different process.
//
// What that does and does not cover: a process killed between the intent
// and the answer leaves an intent with no result, and the rig reports that
// write as indeterminate rather than pretending to know. A machine that
// loses power could in principle tear the last line; a torn tail is
// detected and reported, never silently dropped. The rig kills processes,
// not machines, so it measures durability across process death and says
// nothing about durability across power loss.

// Phase distinguishes the two records every write leaves behind.
type Phase string

const (
	// PhaseIntent is written, and flushed, before the store is asked to do
	// anything.
	PhaseIntent Phase = "intent"

	// PhaseResult is written after the store answers.
	PhaseResult Phase = "result"
)

// Outcome is what the writer was told about its write.
type Outcome string

const (
	// OutcomeCommitted means the writer was told the write is committed.
	// These are the writes the §16 gate is about: every one of them must
	// be readable afterwards.
	OutcomeCommitted Outcome = "committed"

	// OutcomeRefused means the writer was told the write failed. The store
	// must therefore not hold it.
	OutcomeRefused Outcome = "refused"

	// OutcomeIndeterminate means no answer arrived — the owner died with
	// the request in flight, or the transport lost it. The write may or may
	// not have landed, and the rig counts it as neither committed nor
	// refused rather than guessing.
	OutcomeIndeterminate Outcome = "indeterminate"
)

// Record is one line of a writer's ledger.
type Record struct {
	Phase    Phase   `json:"phase"`
	Writer   string  `json:"writer"`
	Seq      int     `json:"seq"`
	Key      string  `json:"key"`
	Payload  string  `json:"payload"`
	Digest   string  `json:"digest"`
	Outcome  Outcome `json:"outcome,omitempty"`
	Hash     string  `json:"hash,omitempty"`
	ErrClass string  `json:"errClass,omitempty"`
	ErrText  string  `json:"errText,omitempty"`
	AtUnixNS int64   `json:"atUnixNs"`
}

// Ledger appends one writer's records to its own file.
type Ledger struct {
	mu     sync.Mutex
	writer string
	file   *os.File
}

// OpenLedger creates the append-only ledger file for a writer.
func OpenLedger(dir, writer string) (*Ledger, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create ledger directory %s: %w", dir, err)
	}
	path := filepath.Join(dir, writer+".jsonl")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create ledger %s: %w", path, err)
	}
	return &Ledger{writer: writer, file: file}, nil
}

// Append writes one record and flushes it to the operating system before
// returning. The flush is what makes an intent recorded before the write
// survive the writer being killed during it.
func (l *Ledger) Append(rec Record) error {
	rec.Writer = l.writer
	rec.AtUnixNS = time.Now().UnixNano()

	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encode ledger record: %w", err)
	}
	line = append(line, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.file.Write(line); err != nil {
		return fmt.Errorf("append to ledger %s: %w", l.file.Name(), err)
	}
	if err := l.file.Sync(); err != nil {
		return fmt.Errorf("flush ledger %s: %w", l.file.Name(), err)
	}
	return nil
}

// Close closes the ledger file.
func (l *Ledger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.file.Close()
}

// WriterLedger is one writer's ledger as read back.
type WriterLedger struct {
	Writer   string
	Records  []Record
	TornTail bool
}

// ReadLedgers reads every writer's ledger in dir.
//
// A last line that does not parse is a tail torn by an unclean kill: it is
// reported through TornTail so that reconciliation can account for the
// write it describes, and never dropped. A line that does not parse
// anywhere else means the ledger itself is untrustworthy, which is a hard
// error — the rig will not report numbers it cannot stand behind.
func ReadLedgers(dir string) (map[string]*WriterLedger, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read ledger directory %s: %w", dir, err)
	}

	ledgers := make(map[string]*WriterLedger)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)

	for _, name := range names {
		path := filepath.Join(dir, name)
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read ledger %s: %w", path, err)
		}

		ledger := &WriterLedger{Writer: strings.TrimSuffix(name, ".jsonl")}
		lines := bytes.Split(raw, []byte{'\n'})
		// A complete record always ends in a newline, so the split leaves an
		// empty final element. Anything else is a partial last line.
		if last := lines[len(lines)-1]; len(last) != 0 {
			ledger.TornTail = true
		}
		lines = lines[:len(lines)-1]

		for i, line := range lines {
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			var rec Record
			if err := json.Unmarshal(line, &rec); err != nil {
				return nil, fmt.Errorf("ledger %s line %d does not parse, so the run cannot be reconciled: %w", path, i+1, err)
			}
			ledger.Records = append(ledger.Records, rec)
		}
		ledgers[ledger.Writer] = ledger
	}
	return ledgers, nil
}

// digestOf is the checksum stored alongside every payload, so that a value
// that comes back changed is detected as changed rather than compared only
// against itself.
func digestOf(payload string) string {
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}
