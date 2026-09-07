package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

func newTransferCommand(operation string) *cobra.Command {
	var flags storeFlags
	var user, resolutionFile string
	behavior := "Publish only the captured committed main hash to remote main, creating it\n" +
		"or advancing it by fast-forward. Never force, publish proposal refs, or\n" +
		"publish the working set. Scan persisted main text/provenance before upload."
	if operation == "pull" {
		behavior = "Fetch remote main, then validate its exact committed schema and scan added\n" +
			"or changed text/provenance before promotion. Equal or contained history is\n" +
			"unchanged. Compatible divergence creates one attributed two-parent merge.\n" +
			"Conflicts display exact base/ours/theirs rows and commit/blame provenance;\n" +
			"main stays unchanged until complete explicit choices pass validation.\n\n" +
			"Use --json to inspect conflicts. Submit --resolve <file> (or - for stdin):\n" +
			"{\"localCommit\":\"<displayed hash>\",\"remoteCommit\":\"<displayed hash>\",\n" +
			" \"choices\":[{\"conflict\":\"<displayed id>\",\"take\":\"ours\"}]}\n" +
			"Every data conflict needs take=ours, theirs, or manual. Manual supplies row\n" +
			"with every writable column, including nulls; omit generated facts.live_key.\n" +
			"For live-fact-key conflicts use take=winner and winner=<displayed row ID>,\n" +
			"or take=manual with that winner and its full row. Losers are superseded,\n" +
			"never deleted. Task done remains done unless the choice sets reopen=true.\n" +
			"Already-satisfied recorded UNIQUE violations are verified and cleared.\n" +
			"Schema, metadata, unknown constraints or unattributed records refuse with\n" +
			"a Dolt/upgrade remedy. Manual identity/provenance changes are forbidden.\n" +
			"All choices commit together as user; changed heads require fresh review.\n" +
			"No transaction remains open while the operator prepares the file.\n\n" +
			"Local tags and their metadata are preserved; remote tags are not fetched.\n" +
			"Fetched objects and tracking refs may remain on refusal. This is no history\n" +
			"scrub: fetched history may contain text matching the local deny-list.\n" +
			"File sources are read without initialization; unsupported journaled sources\n" +
			"require a prepared filesystem or remotesapi remote."
	}
	cmd := &cobra.Command{
		Use:   operation + " [remote]",
		Short: operation + " committed main using a configured remote (default origin)",
		Long: behavior + "\n\n" +
			"Require an initialized current store and a clean main working set without\n" +
			"an active merge/conflict. Reads and transfers use the authenticated owner\n" +
			"when running; memdolt mutations wait during transfer. Foreign Dolt processes\n" +
			"are outside that coordination. Derived indexes and rendered files stay local;\n" +
			"use the existing index status/rebuild workflow after imported text changes.\n\n" +
			"Select one configured remote name, never a URL, branch or refspec. URLs must\n" +
			"be explicit http(s) remotesapi or absolute file URLs without credentials,\n" +
			"query or fragment. --user overrides the validated stored SQL username;\n" +
			"omit both for anonymous access. Only the executing process environment\n" +
			"supplies DOLT_REMOTE_PASSWORD. Restart a running owner with that environment\n" +
			"to change its credentials; caller passwords are never forwarded over IPC.\n\n" +
			"Configure a remote with `memdolt repo remote add <name> <absolute-url>\n" +
			"--dir <repository> [--user <sql-user>]`; inspect `memdolt repo remote list`,\n" +
			"or clone into a fresh --dir. Transfers are submitted once. A lost response\n" +
			"can leave an unknown outcome: inspect local and remote main before retrying.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 && args[0] == "" {
				return errors.New("remote name must not be empty; omit it to select origin")
			}
			if cmd.Flags().Changed("user") && user == "" {
				return errors.New("--user must not be empty; omit it for configured or anonymous access")
			}
			if err := localdolt.RequireExistingTransferStore(flags.dir); err != nil {
				return err
			}
			opts := localdolt.TransferOptions{Remote: "origin", User: user, Author: cliActor}
			if cmd.Flags().Changed("resolve") {
				var err error
				opts.Resolution, err = readPullResolution(cmd, resolutionFile)
				if err != nil {
					return err
				}
			}
			st, err := openCommandStore(cmd.Context(), flags.dir, cliActor)
			if err != nil {
				return err
			}
			if len(args) == 1 {
				opts.Remote = args[0]
			}
			return runTransfer(cmd, st, operation, opts)
		},
	}
	cmd.Flags().StringVar(&user, "user", "", "SQL username; password comes only from DOLT_REMOTE_PASSWORD in the transfer process")
	if operation == "pull" {
		cmd.Flags().StringVar(&resolutionFile, "resolve", "", "complete conflict-resolution JSON file; - reads stdin; bound to the displayed local/remote hashes")
	}
	return flags.bind(cmd)
}

func runTransfer(cmd *cobra.Command, st commandStore, operation string, opts localdolt.TransferOptions) error {
	var result localdolt.TransferResult
	var err error
	if operation == "push" {
		result, err = st.Push(cmd.Context(), opts)
	} else {
		result, err = st.Pull(cmd.Context(), opts)
	}
	// Close before printing success. The result-bearing diagnostic still says
	// what was confirmed if shutdown or output fails after a durable transfer.
	err = errors.Join(err, st.Close())
	if err == nil || operation == "pull" && result.Changed {
		if err != nil {
			result.Error = err.Error()
		}
		lines := []string{
			fmt.Sprintf("%s %s: %s (changed=%t)", result.Operation, result.Remote, result.Status, result.Changed),
			"captured local main: " + result.LocalCommit,
			"remote main: " + result.RemoteCommit,
			"resulting local main: " + result.MainCommit,
		}
		if result.Status == "conflicted" {
			conflicts, encodeErr := json.MarshalIndent(result.Conflicts, "", "  ")
			if encodeErr != nil {
				return encodeErr
			}
			lines = append(lines, string(conflicts), result.Remedy)
		}
		err = errors.Join(err, emit(cmd, result, lines))
		if err == nil && result.Status == "conflicted" {
			return errors.New("pull has unresolved conflicts; " + result.Remedy)
		}
	}
	if err != nil && (result.Status == "changed" || result.Status == "current") {
		return fmt.Errorf("%s %s confirmed %s: local main %s, remote main %s; inspect before retrying: %w", operation, result.Remote, result.Status, result.MainCommit, result.RemoteCommit, err)
	}
	return err
}

func readPullResolution(cmd *cobra.Command, path string) (*localdolt.PullResolution, error) {
	if path == "-" {
		return localdolt.DecodePullResolution(cmd.InOrStdin())
	}
	if path == "" {
		return nil, errors.New("--resolve requires a JSON file or - for stdin")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(filepath.Dir(abs))
	if err != nil {
		return nil, err
	}
	file, err := root.Open(filepath.Base(abs))
	if err != nil {
		return nil, errors.Join(err, root.Close())
	}
	info, err := file.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = errors.New("resolution must be a regular JSON file")
	}
	var resolution *localdolt.PullResolution
	if err == nil {
		resolution, err = localdolt.DecodePullResolution(file)
	}
	return resolution, errors.Join(err, file.Close(), root.Close())
}
