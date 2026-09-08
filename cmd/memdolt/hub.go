package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/kninetimmy/memdolt/internal/hub"
)

func newHubCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use: "hub", Short: "Generate and inspect a private native Linux Dolt deployment",
		Long: "Generate reviewable deployment artifacts or inspect an explicitly selected hub.\n" +
			"No local memory store, daemon, account, firewall or database is created by\n" +
			"these commands. Startup requires Linux, systemd, nftables and native Dolt\n" +
			"1.88.1. See docs/hub-deployment.md and the generated SETUP.md.\n" +
			"SQL listener.host does not confine Dolt's wildcard remotesapi listener.",
	}
	cmd.AddCommand(newHubInitCommand())
	for _, mode := range []string{"status", "preflight", "ready"} {
		cmd.AddCommand(newHubCheckCommand(mode))
	}
	return cmd
}

func newHubInitCommand() *cobra.Command {
	cfg := hub.DefaultConfig()
	var output string
	cmd := &cobra.Command{
		Use: "init", Short: "Write a new nonsecret hub bundle to an explicit local directory",
		Long: "Write hub.json, native Dolt YAML, two systemd units, private.nft and SETUP.md.\n" +
			"--output is a new absolute local directory with an existing parent;\n" +
			"--config-dir is the eventual Linux installation directory. No installation\n" +
			"occurs. Existing identical bundles are unchanged; any different, partial,\n" +
			"linked or foreign destination refuses without replacement. On write failure,\n" +
			"inspect the reported directory and complete files before retrying.\n" +
			"Explicit private IPv4 and IPv6 addresses are required; the boundary protects\n" +
			"both SQL and remotes ports on both families, permitting loopback and the\n" +
			"configured private interface/source prefixes/destination addresses only.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			result, err := hub.Init(output, cfg)
			lines := []string{"hub init: " + result.Status, "output: " + result.Output}
			for _, file := range result.Files {
				lines = append(lines, "written: "+file)
			}
			if result.Error != "" {
				lines = append(lines, "error: "+result.Error)
			}
			return errors.Join(err, emit(cmd, result, lines))
		},
	}
	f := cmd.Flags()
	f.StringVar(&output, "output", "", "absolute local bundle directory (existing parent required)")
	f.StringVar(&cfg.ConfigDir, "config-dir", cfg.ConfigDir, "absolute Linux deployment configuration directory")
	f.StringVar(&cfg.DataDir, "data-dir", cfg.DataDir, "absolute Linux native Dolt data directory")
	f.StringVar(&cfg.DoltPath, "dolt", cfg.DoltPath, "absolute Linux path to checksum-verified native Dolt 1.88.1")
	f.StringVar(&cfg.MemdoltPath, "memdolt", cfg.MemdoltPath, "absolute Linux path to the installed memdolt binary")
	f.StringVar(&cfg.NFTPath, "nft", cfg.NFTPath, "absolute Linux path to nft")
	f.StringVar(&cfg.User, "user", cfg.User, "dedicated unprivileged Linux user/group")
	f.StringVar(&cfg.NetworkService, "network-service", cfg.NetworkService, "private-network systemd service required before startup")
	f.StringVar(&cfg.Interface, "interface", cfg.Interface, "private network interface")
	f.StringVar(&cfg.IPv4, "ipv4", "", "private IPv4 address assigned to the interface (required)")
	f.StringVar(&cfg.IPv6, "ipv6", "", "private ULA IPv6 address assigned to the interface (required)")
	f.StringVar(&cfg.AllowIPv4, "allow-ipv4", cfg.AllowIPv4, "permitted private IPv4 source prefix")
	f.StringVar(&cfg.AllowIPv6, "allow-ipv6", cfg.AllowIPv6, "permitted private IPv6 source prefix")
	f.IntVar(&cfg.SQLPort, "sql-port", cfg.SQLPort, "private SQL TCP port")
	f.IntVar(&cfg.RemotesPort, "remotes-port", cfg.RemotesPort, "protected wildcard remotesapi TCP port")
	f.IntVar(&cfg.ReadySeconds, "ready-seconds", cfg.ReadySeconds, "bounded private-address startup wait (1..120 seconds)")
	return cmd
}

func newHubCheckCommand(mode string) *cobra.Command {
	var config string
	var filesOnly bool
	short := map[string]string{
		"status":    "Observe deployment files, native release, private boundary and TCP listeners",
		"preflight": "Privileged read-only check of deployment files and the applied private boundary",
		"ready":     "Unprivileged version, credential-file and bounded private-address startup check",
	}
	cmd := &cobra.Command{
		Use: mode, Short: short[mode], Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			report, err := hub.Inspect(cmd.Context(), config, mode, filesOnly)
			lines := []string{fmt.Sprintf("hub %s: ok=%t", mode, report.OK)}
			for _, check := range report.Checks {
				lines = append(lines, fmt.Sprintf("%s %s: %s", check.Status, check.Name, check.Detail))
			}
			return errors.Join(err, emit(cmd, report, lines))
		},
	}
	cmd.Flags().StringVar(&config, "config", "", "absolute path to the selected generated hub.json (required)")
	if mode == "preflight" {
		cmd.Flags().BoolVar(&filesOnly, "files-only", false, "validate trusted artifacts before nft setup; does NOT assert applied protection")
	}
	return cmd
}
