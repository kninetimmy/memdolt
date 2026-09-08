package storeipc

import (
	"context"
	"errors"
	"unicode/utf8"

	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

func (s *OwnerStore) CapturePromotion(ctx context.Context, kind, ident string) (localdolt.PromotionRecord, error) {
	if !utf8.ValidString(kind) || !utf8.ValidString(ident) {
		return localdolt.PromotionRecord{}, errors.New("promotion identifier must be valid UTF-8")
	}
	var result localdolt.PromotionRecord
	err := s.operation(ctx, opCapturePromotion, lastChangedArgs{SourceType: kind, SourceID: ident}, &result)
	return result, err
}

func (s *OwnerStore) CaptureRecall(ctx context.Context, opts store.RecallSnapshotOptions) (store.RecallSnapshot, error) {
	for _, value := range append([]string{opts.Query}, opts.SourceTypes...) {
		if !utf8.ValidString(value) {
			return store.RecallSnapshot{}, errors.New("combined recall input must be valid UTF-8")
		}
	}
	var result store.RecallSnapshot
	err := s.operation(ctx, opCaptureRecall, opts, &result)
	return result, err
}
