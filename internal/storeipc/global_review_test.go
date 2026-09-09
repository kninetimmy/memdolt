package storeipc_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kninetimmy/memdolt/internal/memory"
	reviewgate "github.com/kninetimmy/memdolt/internal/review"
	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
	"github.com/kninetimmy/memdolt/internal/storeipc"
)

func TestGlobalReviewOwnerLostReplyPreservesUnknownWithoutReplay(t *testing.T) {
	home := interopTempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	ctx := context.Background()
	var calls atomic.Int32
	var lose atomic.Bool
	base, st, _ := startOwnerConfigured(t, func(st *localdolt.Store) storeipc.ReviewAcceptFunc {
		return func(ctx context.Context, id, expected string, reviewer store.Actor, force bool) (localdolt.AcceptResult, error) {
			calls.Add(1)
			base := filepath.Dir(filepath.Dir(st.DataDir()))
			return reviewgate.AcceptExpected(ctx, st, filepath.Join(base, ".memdolt", "config.toml"), id, expected, reviewer, force)
		}
	}, func(inner http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if lose.Load() && r.URL.Path == storeipc.OperationPath {
				inner.ServeHTTP(httptest.NewRecorder(), r)
				panic(http.ErrAbortHandler)
			}
			inner.ServeHTTP(w, r)
		})
	})
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := localdolt.SetGlobalEnabled(base, true); err != nil {
		t.Fatal(err)
	}
	paths, err := localdolt.GlobalPaths()
	if err != nil {
		t.Fatal(err)
	}
	bootstrap, err := localdolt.New(localdolt.Config{BaseDir: paths.Base(), Actor: memory.UserActor.CommitAuthor()})
	if err != nil {
		t.Fatal(err)
	}
	if err := bootstrap.Open(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := bootstrap.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := bootstrap.Close(); err != nil {
		t.Fatal(err)
	}
	staged, err := st.ProposeDecision(ctx, localdolt.Proposal{Rationale: "terminal approval", Actor: testActor, Target: localdolt.TargetGlobal}, localdolt.Decision{Title: "Keep one native acceptance", Rationale: "lost replies never replay"})
	if err != nil {
		t.Fatal(err)
	}
	routed, err := storeipc.DialOwnerStore(base)
	if err != nil {
		t.Fatal(err)
	}
	if err := routed.Open(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = routed.Close() }()
	if result, err := routed.ReviewAcceptExpected(ctx, staged.ID, staged.Commit, memory.UserActor.CommitAuthor(), true); err == nil || result.Commit != "" || !strings.Contains(err.Error(), "terminal") {
		t.Fatalf("MCP boundary crossed: %+v %v", result, err)
	}
	calls.Store(0)
	lose.Store(true)
	result, err := routed.ReviewAccept(ctx, staged.ID, memory.UserActor.CommitAuthor(), true)
	if err == nil || !errors.Is(err, store.ErrCommitUnknown) || !result.Unknown || result.Commit != "" || result.GlobalStageCommit != "" || calls.Load() != 1 || !strings.Contains(err.Error(), "global") {
		t.Fatalf("lost reply=%+v %v calls=%d", result, err, calls.Load())
	}
	lose.Store(false)
	repeated, err := routed.ReviewAccept(ctx, staged.ID, memory.UserActor.CommitAuthor(), true)
	if err != nil || repeated.Commit == "" || !repeated.AlreadyAccepted || repeated.Unknown || calls.Load() != 2 || repeated.Proposal.Commit != staged.Commit {
		t.Fatalf("explicit retry=%+v %v calls=%d", repeated, err, calls.Load())
	}
	global, err := localdolt.OpenGlobal(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = global.Close() }()
	rows, err := global.Query(ctx, "SELECT COUNT(*) FROM dolt_log WHERE message = ?", "review accept decision "+staged.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var count int
	if !rows.Next() {
		t.Fatal("missing native history")
	}
	if err := rows.Scan(&count); err != nil || count != 1 {
		t.Fatalf("duplicate merges: %d %v", count, err)
	}
}

func TestGlobalReviewOwnerPreservesStagingAndUnknownResultEnvelopes(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "staging confirmed", true: "native unknown"}[unknown], func(t *testing.T) {
			base, st, _ := startOwnerConfigured(t, func(st *localdolt.Store) storeipc.ReviewAcceptFunc {
				return func(ctx context.Context, id, expected string, reviewer store.Actor, force bool) (localdolt.AcceptResult, error) {
					shown, err := st.ProposalDiff(ctx, id)
					if err != nil {
						return localdolt.AcceptResult{}, err
					}
					result := localdolt.AcceptResult{Proposal: shown.Proposal, SourceRetained: true, RowIDs: []string{shown.Changes[0].To["id"]}}
					if unknown {
						return result, store.ErrCommitUnknown
					}
					// The envelope test uses an actual observed native commit as its
					// confirmed hash; the localdolt suite exercises real global staging.
					result.GlobalStageCommit = shown.Proposal.Commit
					return result, errors.New("synthetic failure after observed staging")
				}
			}, nil)
			ctx := context.Background()
			if _, err := st.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			staged, err := st.ProposeFact(ctx, localdolt.Proposal{Rationale: "transport fixture", Actor: testActor, Target: localdolt.TargetGlobal}, localdolt.Fact{Key: "transport.global", Value: "preserve results"})
			if err != nil {
				t.Fatal(err)
			}
			routed, err := storeipc.DialOwnerStore(base)
			if err != nil {
				t.Fatal(err)
			}
			if err := routed.Open(ctx); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = routed.Close() }()
			result, err := routed.ReviewAccept(ctx, staged.ID, memory.UserActor.CommitAuthor(), true)
			if err == nil || result.Proposal.Commit != staged.Commit || result.Commit != "" || !result.SourceRetained || result.Unknown != unknown || errors.Is(err, store.ErrCommitUnknown) != unknown || (result.GlobalStageCommit == "") != unknown {
				t.Fatalf("lost typed outcome: %+v %v", result, err)
			}
		})
	}
}
