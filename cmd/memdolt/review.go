package main

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/kninetimmy/memdolt/internal/layout"
	"github.com/kninetimmy/memdolt/internal/memory"
	reviewgate "github.com/kninetimmy/memdolt/internal/review"
	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

// This file is `memdolt review`, the CLI half of PRD §7's review gate and
// of §11.2's verb list: list, show, accept, reject, expire and stale.
// Everything it decides lives in internal/store/localdolt/review.go, so
// that §11.1's MCP review flow reaches the same code rather than a second
// implementation of the gate (§5.1).

// defaultExpireAfter is how old a proposal branch has to be before
// `review expire` sweeps it. Proposals are an agent's staged work waiting
// on a human; a month is long enough that expiry never races a review in
// progress, and it is the age --older-than overrides.
const defaultExpireAfter = 30 * 24 * time.Hour

// defaultStaleAfter is the age at which `review stale` starts reporting a
// proposal. It is shorter than expiry on purpose: the audit's job is to
// name proposals heading for expiry while accepting them is still an
// option.
const defaultStaleAfter = 7 * 24 * time.Hour

// localCommandStore adapts the embedded store to the same application-level
// accept operation OwnerStore carries over IPC. The review policy stays in
// internal/review.Accept on both sides of that transport boundary.
type localCommandStore struct {
	*localdolt.Store
	baseDir string
}

func (s *localCommandStore) ReviewAccept(
	ctx context.Context,
	id string,
	reviewer store.Actor,
	force bool,
) (localdolt.AcceptResult, error) {
	paths, err := layout.New(s.baseDir)
	if err != nil {
		return localdolt.AcceptResult{}, err
	}
	return reviewgate.Accept(ctx, s.Store, paths.ConfigFile(), id, reviewer, force)
}

func newReviewCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "review",
		Short: "List, inspect, accept and reject the proposals agents have staged",
		Long: "Agents propose and humans promote (PRD §7): a proposal is one commit on its own\n" +
			"proposal/<id> branch, and `review accept` merging that branch into main is the\n" +
			"only path into durable memory in the reviewed lane. Rejecting deletes the branch\n" +
			"and leaves main where it was.",
	}
	cmd.AddCommand(
		newReviewListCommand(),
		newReviewShowCommand(),
		newReviewAcceptCommand(),
		newReviewRejectCommand(),
		newReviewExpireCommand(),
		newReviewStaleCommand(),
	)
	return cmd
}

// proposalList is what the listing verbs print.
type proposalList struct {
	Proposals []localdolt.PendingProposal `json:"proposals"`
}

// proposalLine renders one pending proposal as a single human-readable
// line: what it is, who staged it, how long it has been waiting, and why
// its author thinks it should land.
func proposalLine(proposal localdolt.PendingProposal, now time.Time) string {
	return fmt.Sprintf("%s  %-9s  %-6s  %-18s  %8s  %s",
		proposal.ID, proposal.Kind, proposal.Target, proposal.Actor,
		age(proposal.Age(now)), oneLine(proposal.Rationale))
}

// age renders a proposal's waiting time at the resolution a reviewer cares
// about, which is days once it has been more than one.
func age(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// oneLine collapses free text onto one line, because a rationale may span
// lines and a listing is one line per proposal.
func oneLine(text string) string { return strings.Join(strings.Fields(text), " ") }

func newReviewListCommand() *cobra.Command {
	var flags storeFlags

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the proposals waiting for review, oldest first",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return flags.runStore(cmd, func(ctx context.Context, st commandStore, _ memory.Actor) error {
				pending, err := st.PendingProposals(ctx)
				if err != nil {
					return err
				}
				return emitProposals(cmd, pending, "no proposals are waiting for review")
			})
		},
	}

	return flags.bind(cmd)
}

func newReviewShowCommand() *cobra.Command {
	var flags storeFlags

	cmd := &cobra.Command{
		Use:   "show <id>",
		Short: "Show one proposal and the diff its commit makes",
		Long: "Show one proposal. Because a proposal is exactly one commit on a branch of its\n" +
			"own (PRD §3.1), the diff is a single-commit diff against the main it was cut\n" +
			"from — the whole of what accepting it would change.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return flags.runStore(cmd, func(ctx context.Context, st commandStore, _ memory.Actor) error {
				diff, err := st.ProposalDiff(ctx, args[0])
				if err != nil {
					return err
				}
				return emit(cmd, diff, showLines(diff))
			})
		},
	}

	return flags.bind(cmd)
}

// showLines renders a proposal and its single-commit diff.
func showLines(diff localdolt.ProposalDiff) []string {
	proposal := diff.Proposal
	lines := []string{
		fmt.Sprintf("proposal %s (%s, %s)", proposal.ID, proposal.Kind, proposal.Target),
		fmt.Sprintf("  staged by %s at %s", proposal.Actor, stamp(proposal.StagedAt)),
		fmt.Sprintf("  branch    %s at %s", proposal.Branch, proposal.Commit),
		fmt.Sprintf("  rationale %s", oneLine(proposal.Rationale)),
		"",
	}
	if len(diff.Changes) == 0 {
		return append(lines, "the proposal's commit changes no reviewed-lane row")
	}
	for _, change := range diff.Changes {
		lines = append(lines, fmt.Sprintf("%s %s", change.Type, change.Table))
		for _, line := range changeLines(change) {
			lines = append(lines, "  "+line)
		}
	}
	return lines
}

// changeLines renders one changed row: a column at a time, marking what the
// commit rewrote rather than reprinting the row twice.
func changeLines(change localdolt.ProposalChange) []string {
	columns := map[string]struct{}{}
	for column := range change.From {
		columns[column] = struct{}{}
	}
	for column := range change.To {
		columns[column] = struct{}{}
	}

	lines := make([]string, 0, len(columns))
	for _, column := range sortedKeys(columns) {
		before, hadBefore := change.From[column]
		after, hasAfter := change.To[column]
		switch {
		case hadBefore && hasAfter && before != after:
			lines = append(lines, fmt.Sprintf("~ %s: %s -> %s", column, before, after))
		case hadBefore && hasAfter:
			lines = append(lines, fmt.Sprintf("  %s: %s", column, after))
		case hasAfter:
			lines = append(lines, fmt.Sprintf("+ %s: %s", column, after))
		default:
			lines = append(lines, fmt.Sprintf("- %s: %s", column, before))
		}
	}
	return lines
}

func newReviewAcceptCommand() *cobra.Command {
	var flags storeFlags
	var force bool

	cmd := &cobra.Command{
		Use:   "accept <id>",
		Short: "Merge a proposal into main and delete its branch",
		Long: "Merge a proposal into main, making the row it proposes durable, and delete the\n" +
			"branch. What merges is the one commit the proposal was staged with — the commit\n" +
			"`review show` renders — so a branch carrying anything else is refused rather\n" +
			"than merged past review. Facts and decisions are compared with durable reviewed\n" +
			"rows by the shipped cross-encoder; --force explicitly bypasses only that probe.\n" +
			"The merge is fail-closed (PRD §6.3): a data conflict, a\n" +
			"constraint violation that verification shows is real, or a row memdolt cannot\n" +
			"attribute leaves main exactly where it was and the proposal still pending.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return flags.runStore(cmd, func(ctx context.Context, st commandStore, actor memory.Actor) error {
				result, err := st.ReviewAccept(ctx, args[0], actor.CommitAuthor(), force)
				if err != nil {
					return err
				}
				lines := []string{fmt.Sprintf("accepted %s %s as %s, reviewed by %s",
					result.Proposal.Kind, result.Proposal.ID, result.Commit, actor.Name)}
				for _, cleared := range result.Cleared {
					lines = append(lines, fmt.Sprintf(
						"  cleared %d already-satisfied %s record(s) on %s after verifying the constraint holds",
						len(cleared.Rows), cleared.Constraint, cleared.Table))
				}
				return emit(cmd, result, lines)
			})
		},
	}
	cmd.Flags().BoolVar(&force, "force", false,
		"explicitly accept despite the contradiction probe; deny-list and merge guards still apply")

	return flags.bindWriter(cmd)
}

func newReviewRejectCommand() *cobra.Command {
	var flags storeFlags

	cmd := &cobra.Command{
		Use:   "reject <id>",
		Short: "Delete a proposal's branch, leaving main unchanged",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return flags.runStore(cmd, func(ctx context.Context, st commandStore, _ memory.Actor) error {
				proposal, err := st.RejectProposal(ctx, args[0])
				if err != nil {
					return err
				}
				return emit(cmd, proposal, []string{fmt.Sprintf(
					"rejected %s %s; %s is gone and main is unchanged",
					proposal.Kind, proposal.ID, proposal.Branch)})
			})
		},
	}

	return flags.bind(cmd)
}

func newReviewExpireCommand() *cobra.Command {
	var flags storeFlags
	var olderThan time.Duration

	cmd := &cobra.Command{
		Use:   "expire",
		Short: "Delete proposal branches that have been waiting too long",
		Long: "Delete every proposal branch whose staging commit is older than --older-than\n" +
			"(PRD §12: expiry is a branch age sweep). Nothing on main changes; an expired\n" +
			"proposal is one that was never promoted.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return flags.runStore(cmd, func(ctx context.Context, st commandStore, _ memory.Actor) error {
				expired, err := st.ExpireProposals(ctx, time.Now().Add(-olderThan))
				if err != nil {
					return err
				}
				return emitProposals(cmd, expired,
					fmt.Sprintf("no proposal has been waiting longer than %s", olderThan))
			})
		},
	}
	cmd.Flags().DurationVar(&olderThan, "older-than", defaultExpireAfter,
		"Go duration (for example 720h); default: 720h0m0s (30 days)")

	return flags.bind(cmd)
}

func newReviewStaleCommand() *cobra.Command {
	var flags storeFlags
	var olderThan time.Duration

	cmd := &cobra.Command{
		Use:   "stale",
		Short: "Report proposals that have been waiting too long, changing nothing",
		Long: "Report every proposal older than --older-than. This is an audit: it reads the\n" +
			"proposal branches and writes nothing, so running it never promotes, rejects or\n" +
			"expires anything.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return flags.runStore(cmd, func(ctx context.Context, st commandStore, _ memory.Actor) error {
				pending, err := st.PendingProposals(ctx)
				if err != nil {
					return err
				}
				now := time.Now()
				stale := make([]localdolt.PendingProposal, 0, len(pending))
				for _, proposal := range pending {
					if proposal.Age(now) >= olderThan {
						stale = append(stale, proposal)
					}
				}
				return emitProposals(cmd, stale,
					fmt.Sprintf("no proposal has been waiting longer than %s", olderThan))
			})
		},
	}
	cmd.Flags().DurationVar(&olderThan, "older-than", defaultStaleAfter,
		"Go duration (for example 168h); default: 168h0m0s (7 days)")

	return flags.bind(cmd)
}

// emitProposals prints a set of proposals, or the given line when empty.
func emitProposals(cmd *cobra.Command, proposals []localdolt.PendingProposal, empty string) error {
	now := time.Now()
	lines := make([]string, 0, len(proposals))
	for _, proposal := range proposals {
		lines = append(lines, proposalLine(proposal, now))
	}
	if len(lines) == 0 {
		lines = []string{empty}
	}
	if proposals == nil {
		proposals = []localdolt.PendingProposal{}
	}
	return emit(cmd, proposalList{Proposals: proposals}, lines)
}

// sortedKeys returns a set's members in a stable order, so that a rendered
// diff does not reshuffle its columns between runs.
func sortedKeys(set map[string]struct{}) []string { return slices.Sorted(maps.Keys(set)) }
