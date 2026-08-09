package main

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

// initActor is the identity init's migration commits are attributed to.
// init is a person running a command, so it is PRD §3.1's plain "user"
// rather than an agent. The address is in .invalid (RFC 2606) because
// memdolt asserts an identity for dolt_log and dolt_blame, not a mailbox.
var initActor = store.Actor{Name: "user", Email: "user@memdolt.invalid"}

// initInfo is the payload printed by `memdolt init --json`. An empty
// Applied is how a machine reader sees "already initialized": init applied
// nothing and the Dolt history did not move.
type initInfo struct {
	Store         string        `json:"store"`
	SchemaVersion int           `json:"schemaVersion"`
	Applied       []appliedInfo `json:"applied"`
}

// appliedInfo describes one migration this run applied.
type appliedInfo struct {
	Version int    `json:"version"`
	Name    string `json:"name"`
	Commit  string `json:"commit"`
	Tag     string `json:"tag"`
}

func newInitCommand() *cobra.Command {
	var dir string

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create this repository's memdolt store and bring its schema up to date",
		Long: "Create the embedded Dolt store beneath <dir>/.memdolt and apply every schema\n" +
			"migration it is missing, one Dolt commit each (PRD §6.1, §6.4).\n\n" +
			"init is idempotent: run against a store that is already current, it reports\n" +
			"that and adds nothing to the Dolt history.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInit(cmd, dir)
		},
	}

	cmd.Flags().StringVar(&dir, "dir", ".",
		"repository root to create the store beneath (the store lives in <dir>/.memdolt)")

	return cmd
}

func runInit(cmd *cobra.Command, dir string) (err error) {
	st, err := localdolt.New(localdolt.Config{BaseDir: dir, Actor: initActor})
	if err != nil {
		return err
	}
	if err := st.Open(cmd.Context()); err != nil {
		return err
	}
	// The store holds the single-owner lock (PRD §5.2) until it is closed,
	// and closing is itself a write path, so its failure is reported rather
	// than swallowed.
	defer func() { err = errors.Join(err, st.Close()) }()

	result, err := st.Migrate(cmd.Context())
	if err != nil {
		return err
	}

	info := initInfo{Store: st.DataDir(), SchemaVersion: result.Version, Applied: []appliedInfo{}}
	for _, applied := range result.Applied {
		info.Applied = append(info.Applied, appliedInfo{
			Version: applied.Version,
			Name:    applied.Name,
			Commit:  applied.Commit,
			Tag:     store.MigrationTag(applied.Version),
		})
	}

	if jsonOutput {
		encoded, err := json.Marshal(info)
		if err != nil {
			return fmt.Errorf("encode init result as json: %w", err)
		}
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), string(encoded)); err != nil {
			return fmt.Errorf("write init json: %w", err)
		}
		return nil
	}

	if len(info.Applied) == 0 {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(),
			"memdolt store at %s is already initialized (schema v%d)\n",
			info.Store, info.SchemaVersion); err != nil {
			return fmt.Errorf("write init line: %w", err)
		}
		return nil
	}

	if _, err := fmt.Fprintf(cmd.OutOrStdout(),
		"initialized memdolt store at %s (schema v%d)\n", info.Store, info.SchemaVersion); err != nil {
		return fmt.Errorf("write init line: %w", err)
	}
	for _, applied := range info.Applied {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(),
			"  applied migration %d (%s) as commit %s tagged %s\n",
			applied.Version, applied.Name, applied.Commit, applied.Tag); err != nil {
			return fmt.Errorf("write init line: %w", err)
		}
	}
	return nil
}
