package storeipc_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kninetimmy/memdolt/internal/memory"
	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/storeipc"
)

type confirmedOwnerBackend struct {
	storeipc.Backend
	unknown bool
}

func (s confirmedOwnerBackend) Commit(ctx context.Context, req store.CommitRequest) (store.CommitResult, error) {
	result, err := s.Backend.Commit(ctx, req)
	if err != nil {
		return result, err
	}
	if s.unknown {
		return store.CommitResult{}, store.ErrCommitUnknown
	}
	return result, errors.New("synthetic late owner commit failure")
}

func TestConfirmedOwnerBatchReplyPreservesKnownAndUnknownOutcomes(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "confirmed", true: "unknown"}[unknown], func(t *testing.T) {
			ctx := context.Background()
			base, st, endpoint := startOwner(t)
			if _, err := st.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			if err := endpoint.Close(); err != nil {
				t.Fatal(err)
			}
			routed := transferEndpoint(t, base, st, confirmedOwnerBackend{st, unknown}, nil)
			lanes := memory.New(routed, memory.UserActor)
			note, err := lanes.PrepareNote("synthetic owner batch")
			if err != nil {
				t.Fatal(err)
			}
			hash, err := lanes.CommitNotes(ctx, []memory.Note{note})
			if err == nil || (hash == "") != unknown || errors.Is(err, store.ErrCommitUnknown) != unknown || !strings.Contains(err.Error(), note.ID) {
				t.Fatalf("owner outcome lost: hash=%q error=%v", hash, err)
			}
			if !unknown && (!strings.Contains(err.Error(), hash) || !strings.Contains(err.Error(), "confirmed")) {
				t.Fatal("confirmed outcome absent from error")
			}
			if got := queryInt(t, st, "SELECT COUNT(*) FROM session_notes AS OF 'main' WHERE id = ?", note.ID); got != 1 {
				t.Fatal("owner result fixture did not actually commit")
			}
		})
	}
}
