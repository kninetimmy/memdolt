package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kninetimmy/memdolt/internal/layout"
	"github.com/kninetimmy/memdolt/internal/memory"
	"github.com/kninetimmy/memdolt/internal/retrieval"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

type humanMemoryCommand struct {
	storeFlags
	kind, operation, source, by, prefix, status string
	limit                                       int
	fact                                        localdolt.Fact
	decision                                    localdolt.Decision
}

type factListReport struct {
	Facts []memory.FactRecord `json:"facts"`
}

type decisionListReport struct {
	Decisions []memory.DecisionRecord `json:"decisions"`
}

func newHumanMemoryCommand(kind string) *cobra.Command {
	cmd := &cobra.Command{
		Use: kind, Short: "Assert, inspect and supersede repository " + kind + "s as a human",
		Long: "These are trusted human CLI commands. Writes require the user actor and an\n" +
			"initialized current repository store with a clean main working set. Each\n" +
			"changed write is one user-authored Dolt commit; earlier content stays in\n" +
			"history and superseded rows remain present. Source labels are metadata.\n\n" +
			"Agents must use propose_fact/propose_decision/propose_supersede and human\n" +
			"review. These direct mutations are absent from MCP. No global flags or\n" +
			"promotion backend are available. Lists read committed main only.\n\n" +
			"Decision list includes all statuses by default; --status active filters it.\n" +
			"MCP list_decisions retains its active-only default and default limit.\n\n" +
			"Fact add updates only the live dotted key, retaining id and created_at;\n" +
			"value/source/kind/evidence are replaced and verified_at is stamped. Omitted\n" +
			"kind/evidence clear those fields. Blank kind or decision summary becomes\n" +
			"NULL; nonblank text is preserved. Fact verify changes only verified_at.\n" +
			"Fact verify/supersede require an unambiguous id or key across all rows;\n" +
			"use ids when a key has historical rows. Decisions use ids. Supersession\n" +
			"links existing same-kind rows and refuses self-links, cycles and dangling\n" +
			"replacement chains. Decision supersession also sets status to superseded.\n\n" +
			"Unchanged data adds no empty commit, including a repeated verification in\n" +
			"the same second (the schema's timestamp precision), identical summaries\n" +
			"and identical links. Changed semantic text makes old vectors stale; use\n" +
			"index status/rebuild. Recall checks current source hashes before vectors.\n\n" +
			"The authenticated live owner executes each complete mutation once. Inspect\n" +
			"fact list or decision list --status all after an unknown outcome before\n" +
			"retrying; a confirmed commit remains reported with any later error.",
	}
	operations := []string{"add", "list", "supersede"}
	if kind == "fact" {
		operations = append(operations, "verify")
	} else {
		operations = append(operations, "set-summary")
	}
	for _, operation := range operations {
		flags := humanMemoryCommand{kind: kind, operation: operation}
		uses := map[string]string{
			"add": "add <title> --rationale <text>", "list": "list", "supersede": "supersede <old> --by <new>",
			"verify": "verify <id-or-key>", "set-summary": "set-summary <id> <summary>",
		}
		if kind == "fact" {
			uses["add"] = "add <key> <value>"
		}
		child := &cobra.Command{Use: uses[operation], Short: operation + " repository " + kind + "s", Long: cmd.Long, Args: cobra.ExactArgs(1)}
		switch operation {
		case "list":
			child.Args = cobra.NoArgs
			flags.bind(child)
			child.Flags().IntVar(&flags.limit, "limit", 0, "maximum rows; zero lists all matching rows")
			if kind == "fact" {
				child.Flags().StringVar(&flags.prefix, "prefix", "", "literal dotted key prefix, such as build.; includes superseded rows")
			} else {
				child.Flags().StringVar(&flags.status, "status", "all", "active, superseded, draft, or all")
			}
		case "add":
			flags.bindWriter(child)
			child.Flags().StringVar(&flags.source, "source", "user", "user, git, observed, agent:<id>, or user+agent:<id>; metadata only")
			if kind == "fact" {
				child.Args = cobra.ExactArgs(2)
				child.Flags().StringVar(&flags.fact.Kind, "kind", "", "optional free-form classifier; omitted or blank clears it")
				child.Flags().StringVar(&flags.fact.Evidence, "evidence", "", "optional re-verification pointer; omitted clears it")
			} else {
				child.Flags().StringVar(&flags.decision.Rationale, "rationale", "", "why this decision was made (required)")
				child.Flags().StringVar(&flags.decision.Summary, "summary", "", "optional natural-language summary; blank becomes NULL")
				child.Flags().StringVar(&flags.decision.AlternativesRejected, "alternatives", "", "alternatives considered and rejected")
				child.Flags().StringVar(&flags.decision.Evidence, "evidence", "", "optional re-verification pointer")
			}
		case "supersede":
			flags.bindWriter(child)
			child.Flags().StringVar(&flags.by, "by", "", "existing replacement of the same kind (required)")
		default:
			flags.bindWriter(child)
			if operation == "set-summary" {
				child.Args = cobra.ExactArgs(2)
			}
		}
		if flag := child.Flags().Lookup("actor"); flag != nil {
			flag.Usage = "trusted human attribution: empty or user; agent identities must use the reviewed lane"
		}
		child.RunE = func(cmd *cobra.Command, args []string) error {
			actor, err := memory.NormalizeActor(flags.actor)
			if err != nil {
				return err
			}
			if operation != "list" && actor.Name != memory.UserActor.Name {
				return errors.New("trusted human memory commands require the user actor; agents must propose changes for review")
			}
			if operation == "supersede" && flags.by == "" {
				return errors.New("supersede requires --by <new>")
			}
			if err := localdolt.RequireExistingTransferStore(flags.dir); err != nil {
				return err
			}
			st, err := openCommandStore(cmd.Context(), flags.dir, actor.CommitAuthor())
			if err != nil {
				return err
			}
			return flags.run(cmd, st, actor, args)
		}
		cmd.AddCommand(child)
	}
	return cmd
}

func (f humanMemoryCommand) run(cmd *cobra.Command, st commandStore, actor memory.Actor, args []string) error {
	ctx := cmd.Context()
	if err := requireCurrentSchema(ctx, st); err != nil {
		return errors.Join(err, st.Close())
	}
	if f.operation == "list" {
		var payload any
		var err error
		lines := []string{}
		if f.kind == "fact" {
			paths, pathErr := layout.New(f.dir)
			if pathErr != nil {
				return errors.Join(pathErr, st.Close())
			}
			cfg, configErr := retrieval.LoadConfig(paths.ConfigFile())
			if configErr != nil {
				return errors.Join(configErr, st.Close())
			}
			var facts []memory.FactRecord
			facts, err = memory.ListFacts(ctx, st, f.prefix, f.limit, cfg.FactStaleAfterDays)
			payload = factListReport{Facts: facts}
			for _, fact := range facts {
				lines = append(lines, fmt.Sprintf("%s  %s = %s  [source: %s, stale: %t, superseded by: %s]", fact.ID, fact.Key, fact.Value, fact.Source, fact.Stale, fact.SupersededBy))
			}
		} else {
			var decisions []memory.DecisionRecord
			decisions, err = memory.ListDecisions(ctx, st, f.status, f.limit)
			payload = decisionListReport{Decisions: decisions}
			for _, decision := range decisions {
				lines = append(lines, fmt.Sprintf("%s  %s  %s", decision.ID, decision.Status, decision.Title),
					decision.Rationale, decision.Summary)
			}
		}
		err = errors.Join(err, st.Close())
		if err != nil {
			return err
		}
		if len(lines) == 0 {
			lines = []string{"no " + f.kind + "s"}
		}
		return emit(cmd, payload, lines)
	}
	var result localdolt.HumanMemoryResult
	var err error
	switch f.kind + " " + f.operation {
	case "fact add":
		f.fact.Key, f.fact.Value = args[0], args[1]
		result, err = st.FactAdd(ctx, localdolt.FactAddOptions{Fact: f.fact, Source: f.source, Actor: actor})
	case "fact verify":
		result, err = st.FactVerify(ctx, args[0], actor)
	case "fact supersede":
		result, err = st.FactSupersede(ctx, args[0], f.by, actor)
	case "decision add":
		f.decision.Title = args[0]
		result, err = st.DecisionAdd(ctx, localdolt.DecisionAddOptions{Decision: f.decision, Source: f.source, Actor: actor})
	case "decision set-summary":
		result, err = st.DecisionSetSummary(ctx, args[0], args[1], actor)
	case "decision supersede":
		result, err = st.DecisionSupersede(ctx, args[0], f.by, actor)
	default:
		err = errors.New("unknown human memory command")
	}
	err = errors.Join(err, st.Close())
	if err != nil && result.Commit == "" {
		return err
	}
	if err != nil {
		result.Error = err.Error()
	}
	line := strings.Join([]string{result.Kind, result.ID, result.Status}, " ")
	if result.By != "" {
		line += " by " + result.By
	}
	if result.Commit != "" {
		line += " (commit " + result.Commit + ")"
	}
	err = errors.Join(err, emit(cmd, result, []string{line}))
	if err != nil && result.Commit != "" {
		return fmt.Errorf("%s %s confirmed %s in commit %s; inspect the list before retrying: %w", result.Kind, result.ID, result.Status, result.Commit, err)
	}
	return err
}
