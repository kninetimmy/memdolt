package main

import (
	"encoding/json"
	"fmt"
	"runtime"
	"runtime/debug"

	"github.com/spf13/cobra"
)

// version is the memdolt release version. Release builds set it at link
// time with -ldflags "-X main.version=vX.Y.Z". Local builds (go build,
// go run, go install without ldflags) leave it at "dev"; resolveVersion
// then falls back to whatever the Go toolchain embedded in the binary via
// runtime/debug.ReadBuildInfo (the module's pseudo-version, or the VCS
// revision it was built from).
var version = "dev"

// versionInfo is the payload printed by `memdolt version --json` and used
// to render the human-readable line.
type versionInfo struct {
	Version   string `json:"version"`
	GoVersion string `json:"goVersion"`
	Commit    string `json:"commit,omitempty"`
}

func resolveVersion() versionInfo {
	info := versionInfo{
		Version:   version,
		GoVersion: runtime.Version(),
	}

	buildInfo, ok := debug.ReadBuildInfo()
	if !ok {
		return info
	}

	if info.Version == "dev" && buildInfo.Main.Version != "" && buildInfo.Main.Version != "(devel)" {
		info.Version = buildInfo.Main.Version
	}

	for _, setting := range buildInfo.Settings {
		if setting.Key == "vcs.revision" {
			info.Commit = setting.Value
			break
		}
	}

	return info
}

func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the memdolt version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			info := resolveVersion()

			if jsonOutput {
				encoded, err := json.Marshal(info)
				if err != nil {
					return fmt.Errorf("encode version info as json: %w", err)
				}
				if _, err := fmt.Fprintln(cmd.OutOrStdout(), string(encoded)); err != nil {
					return fmt.Errorf("write version json: %w", err)
				}
				return nil
			}

			if info.Commit != "" {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "memdolt version %s (%s, commit %s)\n",
					info.Version, info.GoVersion, info.Commit); err != nil {
					return fmt.Errorf("write version line: %w", err)
				}
				return nil
			}

			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "memdolt version %s (%s)\n", info.Version, info.GoVersion); err != nil {
				return fmt.Errorf("write version line: %w", err)
			}
			return nil
		},
	}
}
