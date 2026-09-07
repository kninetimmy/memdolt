package main

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

func newExportCommand() *cobra.Command {
	var flags storeFlags
	cmd := &cobra.Command{
		Use: "export <bundle.json>", Short: "Export committed memory and pending proposals as memdolt interop v1",
		Long: "Export one immutable main snapshot and captured pending proposal payloads.\n" +
			"memdolt interop v1 preserves ULIDs and explicit NULLs; non-NULL SQL cells\n" +
			"are strings (including decimal integers and UTC DATETIME seconds). It is\n" +
			"distinct from legacy numeric-ID memhub v1. Neither JSON format is sync.\n" +
			"Documents/chunks, indexes, config, credentials, identity paths and transcripts\n" +
			"are excluded. Only reproducible ordinary repository proposals are supported.\n" +
			"The parent directory must exist outside protected metadata. Reject links,\n" +
			"reparse escapes and unrecognized existing files. Prepare and sync a complete\n" +
			"sibling before native per-file replacement; no stronger crash guarantee.\n" +
			"After a lost reply or leftover .<bundle>.memdolt-export.lock, stop exports\n" +
			"and inspect output before retrying or removing that lock.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return emitInterop(cmd, localdolt.InteropResult{Operation: "export", Status: "refused"}, errors.New("export requires exactly one destination bundle.json"))
			}
			return runInterop(cmd, flags.dir, args[0], false, false)
		},
	}
	return flags.bind(cmd)
}

func newImportCommand() *cobra.Command {
	var flags storeFlags
	var fromMemhub string
	cmd := &cobra.Command{
		Use: "import [bundle.json]", Short: "Import memory into a fresh initialized store; optionally consume memhub v1",
		Long: "Import memdolt interop v1, or `import --from-memhub <export.json>`.\n" +
			"The whole checked bundle is submitted once through the direct or authenticated\n" +
			"owner. Validate all rows, identities, references and denied text before writing.\n" +
			"Require current initialized schema, clean main, no active merge, no durable\n" +
			"memory and no proposal branches. Existing documents and local artifacts remain.\n" +
			"There is no --force, wipe or history rewrite: retain the old store and use\n" +
			"a fresh target. Historical rows retain timestamps and nullable provenance.\n" +
			"Legacy confidence is omitted; writes_log becomes one digest/count annotation\n" +
			"note. Imports and recreated proposals have the current human importer's real\n" +
			"commit authorship/time. Supported pending fact/decision writes remain proposals.\n" +
			"Legacy existing-row supersede, global proposals, duplicate command kinds and\n" +
			"unreproducible old-base native proposals refuse the entire bundle before writes.\n" +
			"Proposal creation may stop after a committed import: inspect confirmed main,\n" +
			"identity mappings and the exact created/remaining prefix. Never auto-replay\n" +
			"an uncertain response. Rebuild the local index and run eval retrieval.\n" +
			"Re-ingest documents only from explicitly selected sources; the bundle is\n" +
			"never modified and metadata paths are never opened. Own-state migration stays\n" +
			"optional: converge first, lowest stakes first, one-week soak, retain old state.",
		RunE: func(cmd *cobra.Command, args []string) error {
			legacy := cmd.Flags().Changed("from-memhub")
			if legacy && (fromMemhub == "" || len(args) != 0) || !legacy && len(args) != 1 {
				return emitInterop(cmd, localdolt.InteropResult{Operation: "import", Status: "refused"}, errors.New("select exactly one native bundle.json or --from-memhub <export.json>"))
			}
			file := fromMemhub
			if !legacy {
				file = args[0]
			}
			return runInterop(cmd, flags.dir, file, true, legacy)
		},
	}
	cmd.Flags().StringVar(&fromMemhub, "from-memhub", "", "read tagged memhub v1 with nullable v0.2.2 note metadata instead of native memdolt interop")
	return flags.bind(cmd)
}

func runInterop(cmd *cobra.Command, dir, file string, importing, legacy bool) error {
	operation := "export"
	if importing {
		operation = "import"
	}
	result := localdolt.InteropResult{Operation: operation, Status: "refused", File: file}
	path, err := filepath.Abs(file)
	if err == nil {
		err = localdolt.RequireExistingTransferStore(dir)
	}
	if err != nil {
		return emitInterop(cmd, result, err)
	}
	st, err := openCommandStore(cmd.Context(), dir, cliActor)
	if err != nil {
		return emitInterop(cmd, result, err)
	}
	if importing {
		result, err = st.ImportMemory(cmd.Context(), localdolt.ImportMemoryOptions{File: path, FromMemhub: legacy})
	} else {
		result, err = st.ExportMemory(cmd.Context(), localdolt.ExportMemoryOptions{File: path})
	}
	return emitInterop(cmd, result, errors.Join(err, st.Close()))
}

func emitInterop(cmd *cobra.Command, result localdolt.InteropResult, err error) error {
	if err != nil {
		result.Error = err.Error()
	}
	lines := []string{result.Operation + ": " + result.Status, "bundle: " + result.File}
	if result.MainCommit != "" {
		lines = append(lines, "confirmed import commit: "+result.MainCommit)
	}
	if result.Written {
		lines = append(lines, "export published from main: "+result.SourceCommit)
	}
	for _, proposal := range result.CreatedProposals {
		lines = append(lines, "created proposal: "+proposal.ID+" commit "+proposal.Commit)
	}
	if len(result.RemainingProposals) != 0 {
		lines = append(lines, fmt.Sprintf("remaining proposal IDs: %v", result.RemainingProposals))
	}
	lines = append(lines, result.Guidance)
	if err != nil {
		lines = append(lines, "error: "+err.Error())
	}
	if outputErr := emit(cmd, result, lines); outputErr != nil {
		err = errors.Join(err, fmt.Errorf("interop reporting failed after status %s, main %s and %d created proposals; inspect before retry: %w", result.Status, result.MainCommit, len(result.CreatedProposals), outputErr))
	}
	return err
}
