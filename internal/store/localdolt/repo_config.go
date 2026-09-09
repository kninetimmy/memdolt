package localdolt

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/BurntSushi/toml"

	"github.com/kninetimmy/memdolt/internal/store"
)

// RepoConfig is machine-local routing policy. Empty Topology preserves native
// remote behavior; explicit local suppresses implicit remote status requests.
type RepoConfig struct {
	RemoteURL              string `toml:"remote_url,omitempty" json:"remoteUrl,omitempty"`
	Topology               string `toml:"topology,omitempty" json:"topology,omitempty"`
	AutoPullOnSessionStart bool   `toml:"auto_pull_on_session_start" json:"autoPullOnSessionStart"`
}

func (cfg RepoConfig) Validate() error {
	switch cfg.Topology {
	case "", "local", "clone":
	case "live":
		return errors.New("[repo] topology=live is unsupported; no local fallback was opened; select local or clone explicitly in .memdolt/config.toml")
	default:
		return errors.New("invalid [repo] topology; use local or clone")
	}
	if cfg.RemoteURL != "" {
		if err := validateRemoteURL(cfg.RemoteURL, ""); err != nil {
			return fmt.Errorf("invalid [repo] remote_url: %w", err)
		}
	}
	if cfg.AutoPullOnSessionStart && cfg.Topology != "clone" {
		return errors.New("[repo] auto_pull_on_session_start requires explicit topology=clone")
	}
	return nil
}

func decodeRepoConfig(raw []byte) (RepoConfig, error) {
	var decoded struct {
		Repo RepoConfig `toml:"repo"`
	}
	metadata, err := toml.Decode(string(raw), &decoded)
	if err != nil {
		// TOML diagnostics can echo a URL containing a password. Only report the
		// consumed surface; operators can inspect their file locally.
		return RepoConfig{}, errors.New("cannot parse repository configuration; inspect .memdolt/config.toml")
	}
	for _, key := range metadata.Undecoded() {
		if len(key) > 0 && key[0] == "repo" {
			return RepoConfig{}, errors.New("unknown [repo] configuration key; use remote_url, topology or auto_pull_on_session_start")
		}
	}
	return decoded.Repo, decoded.Repo.Validate()
}

func ReadRepoConfig(base string) (cfg RepoConfig, err error) {
	root, err := globalConfigRoot(base)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	raw, _, err := readConfigBytes(root)
	if err != nil {
		return cfg, err
	}
	return decodeRepoConfig(raw)
}

// SetRepoConfig replaces only [repo] using the existing protected rooted writer.
// It never contacts a remote, modifies native remotes or changes Dolt history.
func SetRepoConfig(base string, cfg RepoConfig) (changed bool, err error) {
	if err := cfg.Validate(); err != nil {
		return false, err
	}
	if err := RequireExistingTransferStore(base); err != nil {
		return false, err
	}
	root, err := globalConfigRoot(base)
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	raw, mode, err := readConfigBytes(root)
	if err != nil {
		return false, err
	}
	old, err := decodeRepoConfig(raw)
	if err != nil {
		return false, err
	}
	if old == cfg {
		return false, nil
	}
	values := map[string]any{}
	if _, err := toml.Decode(string(raw), &values); err != nil {
		return false, errors.New("cannot parse repository configuration")
	}
	values["repo"] = cfg
	return replaceConfig(root, raw, mode, values)
}

func (s *Store) repoConfig() (RepoConfig, error) {
	if s.cfg.Global {
		return RepoConfig{}, nil
	}
	return ReadRepoConfig(s.paths.Base())
}

// PullOnSessionStart is called once by serve, before publishing MCP or IPC.
// Reusing Pull preserves conflict and uncertain-result policy without replay or
// manufacturing human choices. Ordinary Open and short-lived CLI do not pull.
func (s *Store) PullOnSessionStart(ctx context.Context) (TransferResult, error) {
	return s.pullOnSessionStart(ctx, s.Pull)
}

func (s *Store) pullOnSessionStart(ctx context.Context, pull func(context.Context, TransferOptions) (TransferResult, error)) (TransferResult, error) {
	cfg, err := s.repoConfig()
	if err != nil || !cfg.AutoPullOnSessionStart {
		return TransferResult{}, err
	}
	result, err := pull(ctx, TransferOptions{Author: store.Actor{Name: "memdolt", Email: "memdolt@memdolt.invalid"}})
	if err == nil && result.Status == "conflicted" {
		err = errors.New(result.Remedy)
	}
	if err != nil {
		err = fmt.Errorf("session-start pull did not finish (status %s, confirmed main %s); inspect `memdolt repo status --local` and `memdolt pull --json` before starting another owner; no automatic retry: %w", result.Status, result.MainCommit, err)
	}
	return result, err
}
