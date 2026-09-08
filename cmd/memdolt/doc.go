package main

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/oklog/ulid/v2"
	"github.com/spf13/cobra"

	"github.com/kninetimmy/memdolt/internal/memory"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

type docListReport struct {
	Documents []localdolt.Document `json:"documents"`
}

func newDocCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use: "doc", Short: "Ingest, inspect and remove repository reference documents",
		Long: "Repository documents are a direct lane. Each changed add or removal is one\n" +
			"attributed main commit; document source is user, while the actual normalized\n" +
			"caller authors the commit. Requires an initialized current store.\n\n" +
			"Markdown uses memhub v0.2.0 heading breadcrumbs and a 2000-character soft\n" +
			"paragraph target; single paragraphs and fences remain intact (TEXT bodies\n" +
			"must fit 65535 bytes). Unchanged SHA-256 content adds no commit. Changed\n" +
			"content retains the document id and atomically replaces every chunk. Empty\n" +
			"UTF-8 files are documents with zero chunks. Dirty working sets refuse writes.\n\n" +
			"The first ingestion into an empty documents table visibly enables\n" +
			"[retrieval] include_docs_in_default. Later re-adds preserve user opt-outs.\n" +
			"A late commit, config, close or output failure retains the confirmed document\n" +
			"id and commit hash with an inspection remedy. Lost results mean outcome\n" +
			"unknown, not rollback. Never replay ingestion to repair configuration.\n" +
			"Run index status/rebuild after changed chunks; derived vectors stay local.\n\n" +
			"CLI source paths are explicitly selected, relative to the caller's working\n" +
			"directory, and may be outside the repo. MCP doc_add instead confines files\n" +
			"to the canonical repo root or [doc] allowed_dirs. Both surfaces refuse this\n" +
			"store's owner metadata (.memdolt/server.pid) and aliases before content reads,\n" +
			"independently of deny-list patterns. Paths are local identities,\n" +
			"not portable cross-machine aliases. Store work uses the authenticated live\n" +
			"owner when present. Writes submit once: inspect doc ls/show after a lost\n" +
			"reply before retrying. --global selects the enabled shared replica and\n" +
			"requires the human user actor for writes; see global --help.",
	}
	for _, operation := range []string{"add", "ls", "show", "rm"} {
		var flags storeFlags
		var title string
		uses := map[string]string{"add": "add <file>", "ls": "ls", "show": "show <id-or-path>", "rm": "rm <id-or-path>"}
		shorts := map[string]string{
			"add": "Ingest a UTF-8 Markdown file; --title defaults to its first heading or filename",
			"ls":  "List document metadata, newest first", "show": "Show metadata and ordered chunk breadcrumbs and bodies",
			"rm": "Remove a document and its entire chunk set in one commit",
		}
		child := &cobra.Command{Use: uses[operation], Short: shorts[operation], Long: shorts[operation] + "\n\n" + cmd.Long, Args: cobra.ExactArgs(1)}
		if operation == "ls" {
			child.Args, child.Aliases = cobra.NoArgs, []string{"list"}
		}
		if operation == "rm" {
			child.Aliases = []string{"remove"}
		}
		child.RunE = func(cmd *cobra.Command, args []string) error {
			ident := ""
			if len(args) > 0 {
				ident = args[0]
				if ident == "" {
					return errors.New("document path or id must not be empty")
				}
				_, idErr := ulid.ParseStrict(ident)
				if operation == "add" || idErr != nil {
					var err error
					ident, err = filepath.Abs(ident)
					if err != nil {
						return fmt.Errorf("resolve document path: %w", err)
					}
				}
			}
			actor, err := memory.NormalizeActor(flags.actor)
			if err != nil {
				return err
			}
			if err := localdolt.RequireExistingTransferStore(flags.dir); err != nil {
				return err
			}
			st, err := flags.open(cmd.Context(), actor.CommitAuthor())
			if err != nil {
				return err
			}
			return runDoc(cmd, st, operation, ident, title, actor)
		}
		if operation == "add" || operation == "rm" {
			flags.bindWriter(child)
		} else {
			flags.bind(child)
		}
		if operation == "add" {
			child.Flags().StringVar(&title, "title", "", "document title; blank uses its first heading or filename")
		}
		flags.bindGlobal(child)
		cmd.AddCommand(child)
	}
	return cmd
}

func runDoc(cmd *cobra.Command, st commandStore, operation, ident, title string, actor memory.Actor) error {
	ctx := cmd.Context()
	if operation == "ls" {
		docs, err := st.DocList(ctx)
		err = errors.Join(err, st.Close())
		if err != nil {
			return err
		}
		lines := []string{}
		for _, doc := range docs {
			lines = append(lines, fmt.Sprintf("%s  %s  %d chunks  %d bytes  %s", doc.ID, doc.Title, doc.ChunkCount, doc.ByteLen, doc.Path))
		}
		if len(lines) == 0 {
			lines = append(lines, "no documents")
		}
		return emit(cmd, docListReport{Documents: docs}, lines)
	}
	var result localdolt.DocResult
	var err error
	switch operation {
	case "add":
		result, err = st.DocAdd(ctx, localdolt.DocAddOptions{File: ident, Title: title, Actor: actor})
	case "show":
		result, err = st.DocShow(ctx, ident)
	case "rm":
		result, err = st.DocRemove(ctx, ident, actor)
	default:
		err = errors.New("unknown document operation")
	}
	err = errors.Join(err, st.Close())
	if err != nil && result.Commit == "" && !result.EnabledDefaultRecall {
		return err
	}
	if err != nil {
		result.Error = err.Error()
	}
	lines := []string{"document " + result.Status}
	if doc := result.Document; doc != nil {
		lines = append(lines, fmt.Sprintf("%s  %s  %d chunks  %d bytes", doc.ID, doc.Title, doc.ChunkCount, doc.ByteLen),
			"path: "+doc.Path, "sha256: "+doc.ContentHash, "source: "+doc.Source, "ingested: "+stamp(doc.IngestedAt))
	}
	if result.Commit != "" {
		lines = append(lines, "commit: "+result.Commit)
	}
	for _, chunk := range result.Chunks {
		lines = append(lines, fmt.Sprintf("  [%d] %s  %s", chunk.Ord, chunk.ID, chunk.HeadingPath))
		if operation == "show" {
			lines = append(lines, chunk.Body)
		}
	}
	if result.EnabledDefaultRecall {
		table := "retrieval"
		if global, _ := cmd.Flags().GetBool("global"); global {
			table = "global"
		}
		lines = append(lines, "enabled ["+table+"] include_docs_in_default for this repository")
	}
	err = errors.Join(err, emit(cmd, result, lines))
	if err != nil && result.Commit != "" {
		return fmt.Errorf("document %s confirmed %s in commit %s; inspect doc ls/show before retrying: %w",
			result.Document.ID, result.Status, result.Commit, err)
	}
	if err != nil && result.EnabledDefaultRecall {
		return fmt.Errorf("document default-recall configuration confirmed enabled; inspect the repository config before retrying: %w", err)
	}
	return err
}
