package storeipc

import (
	"context"

	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

func (s *OwnerStore) History(ctx context.Context, opts localdolt.HistoryOptions) (localdolt.HistoryResult, error) {
	if err := opts.Validate(); err != nil {
		return localdolt.HistoryResult{}, err
	}
	var result localdolt.HistoryResult
	if err := s.operation(ctx, opHistory, opts, &result); err != nil {
		return localdolt.HistoryResult{}, err
	}
	return result, nil
}
