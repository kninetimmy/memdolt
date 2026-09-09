package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/kninetimmy/memdolt/internal/ipc"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

func newRepoConfigureCommand(set func(string, localdolt.RepoConfig) (bool, error)) *cobra.Command {
	var dir string
	var selected localdolt.RepoConfig
	cmd := &cobra.Command{
		Use: "configure", Short: "Configure machine-local repository routing with the owner stopped",
		Long: "Set supplied [repo] keys while preserving unrelated TOML settings.\n" +
			"local keeps ordinary status offline; explicitly selected remotes and explicit\n" +
			"push/pull remain available. clone uses the existing native transfer policy.\n" +
			"live is unsupported. With no topology settings the existing native behavior\n" +
			"is preserved. remote_url supplies default origin when it is absent; if native\n" +
			"origin exists both URLs must match exactly. Other named remotes remain\n" +
			"explicit choices. URLs never contain credentials; native remote usernames\n" +
			"and the executing owner's DOLT_REMOTE_PASSWORD keep their existing boundary.\n" +
			"Session-start pull defaults off and requires clone. It runs once per serve\n" +
			"startup before publishing MCP/IPC, never on ordinary CLI opens. No remote\n" +
			"is contacted or rewritten by configure. Confirmed config changes survive\n" +
			"later errors; inspect .memdolt/config.toml before retrying.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) (err error) {
			if !cmd.Flags().Changed("topology") && !cmd.Flags().Changed("remote-url") && !cmd.Flags().Changed("auto-pull-on-session-start") {
				return errors.New("supply --topology, --remote-url or --auto-pull-on-session-start")
			}
			cfg, err := localdolt.ReadRepoConfig(dir)
			if err != nil {
				return err
			}
			if cmd.Flags().Changed("topology") {
				cfg.Topology = selected.Topology
			}
			if cmd.Flags().Changed("remote-url") {
				cfg.RemoteURL = selected.RemoteURL
			}
			if cmd.Flags().Changed("auto-pull-on-session-start") {
				cfg.AutoPullOnSessionStart = selected.AutoPullOnSessionStart
			}
			if err := cfg.Validate(); err != nil {
				return err
			}
			status, _, err := ipc.Probe(cmd.Context(), dir)
			if err != nil {
				return err
			}
			if status == ipc.StatusOwnerLive {
				return errors.New("stop the store owner before repo configure, then restart it to use the selected topology")
			}
			if err := localdolt.RequireExistingTransferStore(dir); err != nil {
				return err
			}
			st, err := localdolt.New(localdolt.Config{BaseDir: dir, Actor: cliActor})
			if err != nil {
				return err
			}
			if err := st.Open(cmd.Context()); err != nil {
				return err
			}
			report := struct {
				localdolt.RepoConfig
				Changed bool   `json:"changed"`
				Error   string `json:"error,omitempty"`
			}{RepoConfig: cfg}
			defer func() {
				err = errors.Join(err, st.Close())
				if err != nil {
					report.Error = err.Error()
				}
				if err == nil || report.Changed {
					err = errors.Join(err, emit(cmd, report, []string{fmt.Sprintf("repository configuration changed=%t; topology=%s; session-start pull=%t", report.Changed, cfg.Topology, cfg.AutoPullOnSessionStart)}))
				}
				if err != nil && report.Changed {
					err = fmt.Errorf("repository configuration change confirmed; inspect .memdolt/config.toml before retrying: %w", err)
				}
			}()
			remotes, err := st.ListRemotes(cmd.Context())
			if err != nil {
				return err
			}
			for _, remote := range remotes {
				if remote.Name == "origin" && cfg.RemoteURL != "" && remote.URL != cfg.RemoteURL {
					return errors.New("[repo] remote_url conflicts with native origin; inspect repo remote list and choose one intended destination")
				}
			}
			report.Changed, err = set(dir, cfg)
			return err
		},
	}
	cmd.Flags().StringVar(&dir, "dir", ".", "repository root whose machine-local routing to configure")
	cmd.Flags().StringVar(&selected.Topology, "topology", "", "local or clone; empty restores native default behavior")
	cmd.Flags().StringVar(&selected.RemoteURL, "remote-url", "", "default origin URL; empty removes the TOML default")
	cmd.Flags().BoolVar(&selected.AutoPullOnSessionStart, "auto-pull-on-session-start", false, "pull once at serve startup; requires clone")
	return cmd
}
