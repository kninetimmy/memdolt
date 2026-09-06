package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

func newTransferCommand(operation string) *cobra.Command {
	var flags storeFlags
	var user string
	behavior := "Publish only the captured committed main hash to remote main, creating it\n" +
		"or advancing it by fast-forward. Never force, publish proposal refs, or\n" +
		"publish the working set. Scan persisted main text/provenance before upload."
	if operation == "pull" {
		behavior = "Fetch remote main, then validate its exact committed schema and scan added\n" +
			"or changed text/provenance before a fast-forward. Equal or already-contained\n" +
			"history is unchanged. Divergence refuses; no automatic merge or migration.\n" +
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
			"No remote editor ships: stop the owner and configure Dolt's remote in\n" +
			"<repository>/.memdolt/dolt/memory using `dolt remote add <name> <absolute-url>`,\n" +
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
			st, err := openCommandStore(cmd.Context(), flags.dir, cliActor)
			if err != nil {
				return err
			}
			opts := localdolt.TransferOptions{Remote: "origin", User: user}
			if len(args) == 1 {
				opts.Remote = args[0]
			}
			return runTransfer(cmd, st, operation, opts)
		},
	}
	cmd.Flags().StringVar(&user, "user", "", "SQL username; password comes only from DOLT_REMOTE_PASSWORD in the transfer process")
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
	if err == nil {
		err = emit(cmd, result, []string{
			fmt.Sprintf("%s %s: %s (changed=%t)", result.Operation, result.Remote, result.Status, result.Changed),
			"captured local main: " + result.LocalCommit,
			"remote main: " + result.RemoteCommit,
			"resulting local main: " + result.MainCommit,
		})
	}
	if err != nil && (result.Status == "changed" || result.Status == "current") {
		return fmt.Errorf("%s %s confirmed %s: local main %s, remote main %s; inspect before retrying: %w", operation, result.Remote, result.Status, result.MainCommit, result.RemoteCommit, err)
	}
	return err
}
