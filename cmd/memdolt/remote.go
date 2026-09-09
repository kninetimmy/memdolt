package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

type remoteListReport struct {
	Remotes []localdolt.Remote `json:"remotes"`
}

func newRepoRemoteCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "remote", Short: "List or add native repository remotes without contacting them"}
	for _, add := range []bool{false, true} {
		var flags storeFlags
		var user string
		child := &cobra.Command{
			Use: "list", Short: "List configured remotes in name order", Args: cobra.ExactArgs(0),
			Long: "List validated native remote names, URLs and optional SQL usernames.\n" +
				"An empty configuration is reported explicitly. No remote is contacted.\n" +
				"Use the authenticated owner when running; never initialize or migrate a\n" +
				"missing or unsupported store. Dirty memory and proposals remain untouched.\n" +
				"Unsafe legacy URLs or parameters refuse without exposing rejected values.\n" +
				"A TOML [repo] remote_url default is separate and is not a native list entry.",
			RunE: func(cmd *cobra.Command, args []string) error {
				var remote *localdolt.Remote
				if add {
					if cmd.Flags().Changed("user") && user == "" {
						return errors.New("--user must not be empty; omit it for anonymous access")
					}
					remote = &localdolt.Remote{Name: args[0], URL: args[1], User: user}
					if err := localdolt.ValidateRemote(*remote); err != nil {
						return err
					}
				}
				if err := localdolt.RequireExistingTransferStore(flags.dir); err != nil {
					return err
				}
				st, err := flags.open(cmd.Context(), cliActor)
				if err != nil {
					return err
				}
				return runRepoRemote(cmd, st, remote)
			},
		}
		if add {
			child.Use, child.Short = "add <name> <absolute-url>", "Add a remote without replacing an occupied name"
			child.Args = cobra.ExactArgs(2)
			child.Long = "Persist one validated URL and optional SQL username in native Dolt remote\n" +
				"configuration. Use explicit http(s) remotesapi URLs with a database path or\n" +
				"an absolute file:/// URL. URL credentials, queries/fragments, ambiguous\n" +
				"encoded paths, arbitrary parameters and refspecs are refused. File URLs\n" +
				"cannot have --user. No remote contact, existing file target or password is\n" +
				"required. Transfers alone read DOLT_REMOTE_PASSWORD from their executing\n" +
				"process environment; caller passwords never cross owner IPC.\n\n" +
				"Use the authenticated owner when running; coordinate with transfers and\n" +
				"memory mutations. Foreign Dolt processes are outside that coordination.\n" +
				"Never initialize/migrate a missing or unsupported store, replace a remote,\n" +
				"commit/discard dirty memory, or change proposals, tags or local artifacts.\n" +
				"Submit once; if completion is uncertain, inspect `memdolt repo remote list`\n" +
				"before retrying. Remote status/diff, merges and hub setup remain separate.\n" +
				"Adding origin refuses a conflicting [repo] remote_url default; other\n" +
				"explicit names retain their independent selection. Neither config is rewritten."
			child.Flags().StringVar(&user, "user", "", "SQL username to store; no password is needed or stored for configuration")
		}
		flags.bindGlobal(child)
		cmd.AddCommand(flags.bind(child))
	}
	return cmd
}

func runRepoRemote(cmd *cobra.Command, st commandStore, remote *localdolt.Remote) error {
	var added localdolt.Remote
	var report remoteListReport
	var err error
	if remote == nil {
		report.Remotes, err = st.ListRemotes(cmd.Context())
	} else {
		added, err = st.AddRemote(cmd.Context(), *remote)
	}
	err = errors.Join(err, st.Close())
	if err == nil {
		if remote != nil {
			err = emit(cmd, added, []string{"added remote: " + remoteLine(added)})
		} else {
			lines := []string{"no remotes configured"}
			if len(report.Remotes) != 0 {
				lines = []string{}
				for _, configured := range report.Remotes {
					lines = append(lines, remoteLine(configured))
				}
			}
			err = emit(cmd, report, lines)
		}
	}
	if err != nil && added.Name != "" {
		return fmt.Errorf("remote %s confirmed persisted; inspect `memdolt repo remote list` before retrying: %w", added.Name, err)
	}
	return err
}

func remoteLine(remote localdolt.Remote) string {
	line := remote.Name + "  " + remote.URL
	if remote.User != "" {
		line += "  user=" + remote.User
	}
	return line
}
