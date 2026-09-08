package localdolt

import (
	"context"
	"errors"

	"github.com/kninetimmy/memdolt/internal/store"
)

// CaptureRecall serializes participating writers and refuses if foreign main
// moved while the existing committed readers ran. It never retries a capture.
func (s *Store) CaptureRecall(ctx context.Context, opts store.RecallSnapshotOptions) (result store.RecallSnapshot, err error) {
	s.proposalMu.Lock()
	defer s.proposalMu.Unlock()
	conn, head, err := s.initializedMainConn(ctx, "combined recall capture")
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, conn.Close()) }()
	if err := validateTransferSchema(ctx, conn, head); err != nil {
		return result, err
	}
	result.Commit = head
	result.Sources, err = s.RecallSources(ctx)
	if err != nil {
		return result, err
	}
	result.Embeddings, err = s.EmbeddingSources(ctx)
	if err != nil {
		return result, err
	}
	result.Lexical, err = s.RecallFTS(ctx, opts.Query, opts.SourceTypes)
	if err != nil {
		return result, err
	}
	if opts.Provenance {
		result.Provenance = map[string]*store.CommitProvenance{}
		for _, source := range result.Sources {
			changed, err := s.LastChanged(ctx, source.SourceType, source.SourceID)
			if err != nil {
				return result, err
			}
			result.Provenance[source.SourceType+"/"+source.SourceID] = changed
		}
	}
	current, err := branchHead(ctx, conn, MainBranch)
	if err != nil || current != head {
		return store.RecallSnapshot{}, errors.Join(errors.New("main changed while capturing combined recall; no coherent scope snapshot is available"), err)
	}
	return result, nil
}
