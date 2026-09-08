package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/kninetimmy/memdolt/internal/layout"
	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

const globalHelp = "Global memory is one shareable Dolt replica at ~/.memdolt/global/.memdolt/dolt/memory.\n" +
	"Ownership, remotes and vectors stay beside it under global/.memdolt; vectors\n" +
	"never sync. --dir selects the calling repository's policy. Enablement defaults\n" +
	"off; disable preserves all data. Missing replicas require explicit bootstrap:\n\n" +
	"  memdolt global enable --dir <repository>\n" +
	"  memdolt global init --dir <repository>\n" +
	"  memdolt repo remote add origin <remote-url> --global --dir <repository>\n" +
	"  memdolt push --global --dir <repository>\n\n" +
	"On another client, enable then use global clone <remote-url> instead of init.\n" +
	"Use repo status, repo remote list, push, pull and index status/rebuild with\n" +
	"--global. Transfer authentication, schema checks and conflict choices are the\n" +
	"ordinary repository workflow. DOLT_REMOTE_PASSWORD stays in this process.\n\n" +
	"Global operations take the shared replica's exclusive lock. Contention is a\n" +
	"visible refusal; stop the active operation/owner and inspect before retrying.\n" +
	"No competing embedded engine or uncertain-write replay is attempted.\n\n" +
	"Enable/disable checks configuration and global paths before changing the flag.\n" +
	"A later failure reports any confirmed change; inspect that repository's\n" +
	".memdolt/config.toml before retrying, including after an output failure.\n\n" +
	"fact/decision add --global and promote <id> --global are trusted human writes.\n" +
	"Promotion copies a committed live record with a fresh id and preserves NULLs;\n" +
	"existing live global fact keys refuse promotion. Explicit fact add --global\n" +
	"updates that live key. Decision title collisions keep both rows and report ids.\n" +
	"doc add/list/show/remove --global uses the same scoped document operations.\n" +
	"Each successful global add enables [global] include_docs_in_default in its\n" +
	"calling repository, including unchanged files populated by another repository.\n\n" +
	"Enabled CLI/MCP recall uses one candidate pool and rerank pass, active repo\n" +
	"retrieval knobs and independent repo/global default docs. Hits retain scope\n" +
	"and captured commit metadata, even when keys or ids coincide. Only facts,\n" +
	"decisions and documents join global recall. Disabled recall stays repo-only.\n" +
	"Global proposal acceptance remains unimplemented: inspect `memdolt review`;\n" +
	"existing refusal/terminal remedies remain. No accepting MCP path is added."

type globalStatusReport struct {
	localdolt.GlobalConfig
	Path    string                      `json:"path"`
	Status  string                      `json:"status"`
	Changed bool                        `json:"changed"`
	Replica *localdolt.RepoStatusReport `json:"replica,omitempty"`
	Error   string                      `json:"error,omitempty"`
}

func newGlobalCommand() *cobra.Command {
	return newGlobalCommandWithSetter(localdolt.SetGlobalEnabled)
}

func newGlobalCommandWithSetter(setEnabled func(string, bool) (bool, error)) *cobra.Command {
	cmd := &cobra.Command{Use: "global", Short: "Opt into, bootstrap and inspect the shared global Dolt replica", Long: globalHelp}
	for _, operation := range []string{"enable", "disable", "status"} {
		var flags storeFlags
		child := &cobra.Command{Use: operation, Short: operation + " global memory for this repository", Long: globalHelp, Args: cobra.NoArgs}
		child.RunE = func(cmd *cobra.Command, _ []string) (err error) {
			var report globalStatusReport
			report.GlobalConfig, err = localdolt.ReadGlobalConfig(flags.dir)
			if err != nil {
				return err
			}
			paths, err := localdolt.GlobalPaths()
			if err != nil {
				return err
			}
			report.Path, report.Status = paths.DoltDataDir(), "disabled (replica not opened)"
			defer func() {
				if err != nil && !report.Changed && operation != "status" {
					return
				}
				confirmation := fmt.Sprintf("global configuration confirmed enabled=%t for %s; inspect its .memdolt/config.toml before retrying", operation == "enable", flags.dir)
				if err != nil {
					if report.Changed {
						report.Status = "changed; follow-up failed"
						err = fmt.Errorf("%s: %w", confirmation, err)
					}
					report.Error = err.Error()
				}
				outputErr := emit(cmd, report, []string{fmt.Sprintf("global memory: %s (enabled=%t)", report.Status, report.Enabled), "store: " + report.Path, report.Error})
				if outputErr != nil && report.Changed {
					outputErr = fmt.Errorf("%s: %w", confirmation, outputErr)
				}
				err = errors.Join(err, outputErr)
			}()
			if operation != "status" {
				report.Changed, err = setEnabled(flags.dir, operation == "enable")
				if report.Changed {
					report.Enabled = operation == "enable"
				}
				if err != nil {
					return err
				}
				cfg, readErr := localdolt.ReadGlobalConfig(flags.dir)
				if readErr != nil {
					return readErr
				}
				report.GlobalConfig = cfg
			}
			if report.Enabled {
				report.Status = "enabled; use global status to inspect the replica"
				if operation == "status" {
					st, openErr := localdolt.OpenGlobal(cmd.Context(), flags.dir)
					err = openErr
					if err == nil {
						var status localdolt.RepoStatusReport
						status, err = st.RepoStatus(cmd.Context(), localdolt.RepoStatusOptions{Local: true})
						report.Replica = &status
						err = errors.Join(err, st.Close())
					}
					report.Status = "available"
					if err != nil {
						report.Status, report.Error = "unavailable", err.Error()
					}
				}
			}
			return err
		}
		cmd.AddCommand(flags.bind(child))
	}
	for _, child := range []*cobra.Command{newInitCommand(), newCloneCommand()} {
		run := child.RunE
		child.Long = globalHelp
		child.RunE = func(cmd *cobra.Command, args []string) error {
			repo, err := cmd.Flags().GetString("dir")
			if err != nil {
				return err
			}
			cfg, err := localdolt.ReadGlobalConfig(repo)
			if err != nil {
				return err
			}
			if !cfg.Enabled {
				return errors.New("global memory is disabled; run global enable --dir <repository> first")
			}
			paths, err := localdolt.GlobalPaths()
			if err != nil {
				return err
			}
			if err := cmd.Flags().Set("dir", paths.Base()); err != nil {
				return err
			}
			return run(cmd, args)
		}
		cmd.AddCommand(child)
	}
	return cmd
}

func (f *storeFlags) bindGlobal(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&f.global, "global", false, "select the enabled global replica using this repository's policy; refuse competing owners")
}

func (f *storeFlags) open(ctx context.Context, actor store.Actor) (commandStore, error) {
	if f.global {
		st, err := localdolt.OpenGlobal(ctx, f.dir)
		if err != nil {
			return nil, err
		}
		return &localCommandStore{Store: st, baseDir: f.dir}, nil
	}
	return openCommandStore(ctx, f.dir, actor)
}

func (f *storeFlags) paths() (layout.Paths, error) {
	if f.global {
		return localdolt.GlobalPaths()
	}
	return layout.New(f.dir)
}
