package main

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/kninetimmy/memdolt/internal/ipc"
	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

// initInfo is the payload printed by `memdolt init --json`. Before identity
// adoption, empty Applied meant history did not move. Now identityCommit
// independently reports the explicit identity write; migrations stay in Applied.
type initInfo struct {
	localdolt.IdentityResult
	Store         string        `json:"store"`
	SchemaVersion int           `json:"schemaVersion"`
	Applied       []appliedInfo `json:"applied"`
	Error         string        `json:"error,omitempty"`
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
			"migration it is missing, one Dolt commit each (PRD §6.1, §6.2, §6.4).\n\n" +
			"init is idempotent: run against a store that is already current, it reports\n" +
			"that and adds nothing to the Dolt history. New repositories record a Git-origin\n" +
			"project identity in one attributed commit. Local-only/no-origin use stays valid.\n" +
			"Existing unidentified stores require --adopt-identity after reviewing origin;\n" +
			"changed origins and identity collisions refuse reassignment. Stop the owner\n" +
			"first. Adoption requires clean main and preserves pending proposal refs.\n" +
			"Confirmed identity/migration commits survive later errors; inspect history\n" +
			"before retrying an unknown outcome. Configure routing with repo configure.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInit(cmd, dir)
		},
	}

	cmd.Flags().StringVar(&dir, "dir", ".",
		"repository root to create the store beneath (the store lives in <dir>/.memdolt)")
	cmd.Flags().Bool("adopt-identity", false, "explicitly record the reviewed Git origin on an existing unidentified store")

	return cmd
}

func runInit(cmd *cobra.Command, dir string) (err error) {
	status, _, err := ipc.Probe(cmd.Context(), dir)
	if err != nil {
		return fmt.Errorf("check for a live store owner before init: %w", err)
	}
	if status == ipc.StatusOwnerLive {
		return errors.New("memdolt init cannot migrate a store while its owner is running; stop the owner and rerun `memdolt init`")
	}

	global := cmd.Parent() != nil && cmd.Parent().Name() == "global"
	adopt, err := cmd.Flags().GetBool("adopt-identity")
	if err != nil {
		return err
	}
	if global && adopt {
		return errors.New("global stores do not adopt repository Git identity")
	}
	st, err := localdolt.New(localdolt.Config{BaseDir: dir, Actor: cliActor, Global: global})
	if err != nil {
		return err
	}
	if err := st.Open(cmd.Context()); err != nil {
		return err
	}
	// The store holds the single-owner lock (PRD §5.2) until it is closed,
	// and closing is itself a write path, so its failure is reported rather
	// than swallowed.
	info := initInfo{Store: st.DataDir(), Applied: []appliedInfo{}}
	defer func() {
		err = errors.Join(err, st.Close())
		if err != nil {
			info.Error = err.Error()
		}
		if err == nil || info.Commit != "" || len(info.Applied) != 0 {
			err = errors.Join(err, emitInit(cmd, info))
		}
		if err != nil && (info.Commit != "" || len(info.Applied) != 0) {
			commits := []string{}
			for _, applied := range info.Applied {
				commits = append(commits, applied.Commit)
			}
			if info.Commit != "" {
				commits = append(commits, info.Commit)
			}
			err = fmt.Errorf("init confirmed commits %v; inspect meta and Dolt history before retrying: %w", commits, err)
		}
	}()
	before, err := st.SchemaVersion(cmd.Context())
	if err != nil {
		return err
	}
	result, err := st.Migrate(cmd.Context())
	info.SchemaVersion = result.Version
	for _, applied := range result.Applied {
		info.Applied = append(info.Applied, appliedInfo{
			Version: applied.Version,
			Name:    applied.Name,
			Commit:  applied.Commit,
			Tag:     store.MigrationTag(applied.Version),
		})
	}
	if err != nil {
		return err
	}
	info.IdentityResult, err = st.InitializeIdentity(cmd.Context(), adopt || before == 0)
	return err
}

func emitInit(cmd *cobra.Command, info initInfo) error {
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
	if info.ProjectID != "" {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "project: %s (hub database %s; identity commit %s)\n", info.ProjectID, info.Database, info.Commit); err != nil {
			return err
		}
	}
	if info.Error != "" {
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), "error: "+info.Error); err != nil {
			return err
		}
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
