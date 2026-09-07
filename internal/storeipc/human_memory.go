package storeipc

import (
	"context"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/kninetimmy/memdolt/internal/memory"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

type humanIdentityArgs struct {
	ID      string       `json:"id"`
	By      string       `json:"by,omitempty"`
	Summary string       `json:"summary,omitempty"`
	Actor   memory.Actor `json:"actor"`
}

func (h *handler) humanMemoryOperation(ctx context.Context, req operationRequest) (localdolt.HumanMemoryResult, error) {
	switch req.Operation {
	case opFactAdd:
		args, err := operationArgs[localdolt.FactAddOptions](req.Args)
		if err != nil {
			return localdolt.HumanMemoryResult{}, err
		}
		return h.store.FactAdd(ctx, args)
	case opDecisionAdd:
		args, err := operationArgs[localdolt.DecisionAddOptions](req.Args)
		if err != nil {
			return localdolt.HumanMemoryResult{}, err
		}
		return h.store.DecisionAdd(ctx, args)
	default:
		args, err := operationArgs[humanIdentityArgs](req.Args)
		if err != nil {
			return localdolt.HumanMemoryResult{}, err
		}
		switch req.Operation {
		case opFactVerify:
			return h.store.FactVerify(ctx, args.ID, args.Actor)
		case opFactSupersede:
			return h.store.FactSupersede(ctx, args.ID, args.By, args.Actor)
		case opDecisionSummary:
			return h.store.DecisionSetSummary(ctx, args.ID, args.Summary, args.Actor)
		case opDecisionSupersede:
			return h.store.DecisionSupersede(ctx, args.ID, args.By, args.Actor)
		default:
			return localdolt.HumanMemoryResult{}, errors.New("unknown human memory operation")
		}
	}
}

func (s *OwnerStore) FactAdd(ctx context.Context, opts localdolt.FactAddOptions) (localdolt.HumanMemoryResult, error) {
	return s.humanMemoryMutation(ctx, opFactAdd, opts)
}

func (s *OwnerStore) FactVerify(ctx context.Context, ident string, actor memory.Actor) (localdolt.HumanMemoryResult, error) {
	return s.humanMemoryMutation(ctx, opFactVerify, humanIdentityArgs{ID: ident, Actor: actor})
}

func (s *OwnerStore) FactSupersede(ctx context.Context, old, by string, actor memory.Actor) (localdolt.HumanMemoryResult, error) {
	return s.humanMemoryMutation(ctx, opFactSupersede, humanIdentityArgs{ID: old, By: by, Actor: actor})
}

func (s *OwnerStore) DecisionAdd(ctx context.Context, opts localdolt.DecisionAddOptions) (localdolt.HumanMemoryResult, error) {
	return s.humanMemoryMutation(ctx, opDecisionAdd, opts)
}

func (s *OwnerStore) DecisionSetSummary(ctx context.Context, id, summary string, actor memory.Actor) (localdolt.HumanMemoryResult, error) {
	return s.humanMemoryMutation(ctx, opDecisionSummary, humanIdentityArgs{ID: id, Summary: summary, Actor: actor})
}

func (s *OwnerStore) DecisionSupersede(ctx context.Context, old, by string, actor memory.Actor) (localdolt.HumanMemoryResult, error) {
	return s.humanMemoryMutation(ctx, opDecisionSupersede, humanIdentityArgs{ID: old, By: by, Actor: actor})
}

func (s *OwnerStore) humanMemoryMutation(ctx context.Context, operation string, args any) (localdolt.HumanMemoryResult, error) {
	// encoding/json replaces invalid UTF-8. Refuse it before encoding so the
	// owner receives exactly the text that the direct path would validate.
	var text []string
	switch input := args.(type) {
	case localdolt.FactAddOptions:
		text = []string{input.Fact.Key, input.Fact.Value, input.Fact.Kind, input.Fact.Evidence, input.Source, input.Actor.Name, input.Actor.Raw}
	case localdolt.DecisionAddOptions:
		text = []string{input.Decision.Title, input.Decision.Rationale, input.Decision.Summary, input.Decision.AlternativesRejected, input.Decision.Evidence, input.Source, input.Actor.Name, input.Actor.Raw}
	case humanIdentityArgs:
		text = []string{input.ID, input.By, input.Summary, input.Actor.Name, input.Actor.Raw}
	}
	for _, value := range text {
		if !utf8.ValidString(value) {
			return localdolt.HumanMemoryResult{}, errors.New("human memory text must be valid UTF-8")
		}
	}
	var result localdolt.HumanMemoryResult
	if err := s.operation(ctx, operation, args, &result); err != nil {
		return localdolt.HumanMemoryResult{}, fmt.Errorf("human memory owner response lost or unavailable; outcome unknown; inspect `memdolt fact list` or `memdolt decision list --status all` before retrying: %w", err)
	}
	if result.Error != "" {
		return result, errors.New(result.Error)
	}
	return result, nil
}
