// Package review owns application-level review operations shared by the CLI
// and MCP surfaces (PRD §5.1, §7).
package review

import (
	"context"

	"github.com/kninetimmy/memdolt/internal/embedding"
	"github.com/kninetimmy/memdolt/internal/retrieval"
	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

// Accept promotes a repository proposal through localdolt's single review
// gate. Model configuration is validated before the gate may open the shipped
// cross-encoder; the fixed contradiction threshold is not configurable.
func Accept(
	ctx context.Context,
	st *localdolt.Store,
	configPath, id string,
	reviewer store.Actor,
	force bool,
) (localdolt.AcceptResult, error) {
	return accept(ctx, st, configPath, id, "", reviewer, force)
}

// AcceptExpected is the MCP review variant: expectedCommit is the exact
// proposal commit the elicitation displayed. CLI acceptance calls Accept and
// keeps its established current-head behavior.
func AcceptExpected(
	ctx context.Context,
	st *localdolt.Store,
	configPath, id, expectedCommit string,
	reviewer store.Actor,
	force bool,
) (localdolt.AcceptResult, error) {
	return accept(ctx, st, configPath, id, expectedCommit, reviewer, force)
}

func accept(
	ctx context.Context,
	st *localdolt.Store,
	configPath, id, expectedCommit string,
	reviewer store.Actor,
	force bool,
) (localdolt.AcceptResult, error) {
	return st.AcceptProposal(ctx, id, reviewer, localdolt.AcceptOptions{
		Force:          force,
		ExpectedCommit: expectedCommit,
		ValidateContradictionConfig: func() error {
			_, err := retrieval.LoadConfig(configPath)
			return err
		},
		OpenContradictionScorer: func(ctx context.Context) (localdolt.ContradictionScorer, error) {
			return embedding.Open(ctx, embedding.Options{})
		},
	})
}
