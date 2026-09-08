package localdolt

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/kninetimmy/memdolt/internal/layout"
	"github.com/kninetimmy/memdolt/internal/memory"
	"github.com/kninetimmy/memdolt/internal/store"
)

// GlobalConfig belongs to the calling repository, never to the shared replica.
type GlobalConfig struct {
	Enabled              bool `toml:"enabled" json:"enabled"`
	IncludeDocsInDefault bool `toml:"include_docs_in_default" json:"includeDocsInDefault"`
}

// GlobalPaths reuses the repository layout beneath ~/.memdolt/global. Thus
// durable data lives in global/.memdolt/dolt/memory, beside local ownership,
// configuration and vectors. Resolving or inspecting these paths creates nothing.
func GlobalPaths() (layout.Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return layout.Paths{}, fmt.Errorf("resolve global memory home: %w", err)
	}
	if !filepath.IsAbs(home) || strings.HasPrefix(home, `\\`) || strings.HasPrefix(home, "//") || strings.ContainsAny(home, "?%\x00\r\n") {
		return layout.Paths{}, errors.New("global memory requires an absolute local home directory without ambiguous path characters")
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		return layout.Paths{}, fmt.Errorf("resolve global memory home: %w", err)
	}
	if !filepath.IsAbs(home) || strings.HasPrefix(home, `\\`) || strings.HasPrefix(home, "//") || strings.ContainsAny(home, "?%\x00\r\n") {
		return layout.Paths{}, errors.New("resolved global memory home must remain an unambiguous absolute local directory")
	}
	paths, err := layout.New(filepath.Join(home, layout.DirName, "global"))
	if err != nil {
		return layout.Paths{}, err
	}
	for _, path := range []string{paths.Base(), paths.Dir(), paths.DoltDataDir(), paths.LockFile(), paths.PidFile(), paths.ConfigFile(), paths.EmbeddingsFile(), filepath.Join(paths.DoltDataDir(), DatabaseName, ".dolt", "noms", "manifest")} {
		if err := globalManagedPath(home, path); err != nil {
			return layout.Paths{}, err
		}
	}
	return paths, nil
}

func globalManagedPath(base, path string) error {
	rel, err := filepath.Rel(base, path)
	if err != nil || !filepath.IsLocal(rel) || strings.Contains(rel, ":") {
		return errors.New("global memory path escapes its selected local root")
	}
	for path != base {
		info, err := os.Lstat(path)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("inspect global memory path: %w", err)
		}
		if err == nil && globalUnsafeLink(info) {
			return errors.New("global memory refuses links or reparse points in managed paths")
		}
		path = filepath.Dir(path)
	}
	return nil
}

func globalConfigRoot(repo string) (*os.Root, error) {
	paths, err := layout.New(repo)
	if err != nil {
		return nil, err
	}
	base, err := filepath.EvalSymlinks(paths.Base())
	if err != nil {
		return nil, fmt.Errorf("resolve global policy repository: %w", err)
	}
	dir := filepath.Join(base, layout.DirName)
	if err := globalManagedPath(base, filepath.Join(dir, layout.ConfigFileName)); err != nil {
		return nil, err
	}
	return os.OpenRoot(dir)
}

func ReadGlobalConfig(repo string) (cfg GlobalConfig, err error) {
	root, err := globalConfigRoot(repo)
	if os.IsNotExist(err) {
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
	var decoded struct {
		Global GlobalConfig `toml:"global"`
	}
	metadata, err := toml.Decode(string(raw), &decoded)
	if err != nil {
		return cfg, fmt.Errorf("parse global configuration: %w", err)
	}
	for _, key := range metadata.Undecoded() {
		if len(key) > 0 && key[0] == "global" {
			return cfg, errors.New("unknown [global] configuration key; use enabled or include_docs_in_default")
		}
	}
	return decoded.Global, nil
}

// SetGlobalEnabled changes only this repository's opt-in. Bootstrap is explicit;
// disabling never removes the replica, documents, vectors or configuration.
func SetGlobalEnabled(repo string, enabled bool) (changed bool, err error) {
	if err := RequireExistingTransferStore(repo); err != nil {
		return false, err
	}
	root, err := globalConfigRoot(repo)
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	return setDocumentConfigBool(root, "global", "enabled", enabled)
}

func requireGlobalEnabled(repo string) error {
	cfg, err := ReadGlobalConfig(repo)
	if err != nil {
		return err
	}
	if !cfg.Enabled {
		return errors.New("global memory is disabled for this repository; run `memdolt global enable --dir <repository>` first")
	}
	return nil
}

// OpenGlobal never creates/migrates a missing replica and never routes around an
// active owner. The ordinary lock gives all repository CLI/MCP processes the
// same visible contention refusal, before any embedded engine can open.
func OpenGlobal(ctx context.Context, repo string) (*Store, error) {
	if err := requireGlobalEnabled(repo); err != nil {
		return nil, err
	}
	paths, err := GlobalPaths()
	if err != nil {
		return nil, err
	}
	if err := RequireExistingTransferStore(paths.Base()); err != nil {
		return nil, fmt.Errorf("global replica unavailable; explicitly run `memdolt global init` or `memdolt global clone <remote-url>`: %w", err)
	}
	st, err := New(Config{BaseDir: paths.Base(), Actor: memory.UserActor.CommitAuthor()})
	if err != nil {
		return nil, err
	}
	policy, err := layout.New(repo)
	if err != nil {
		return nil, err
	}
	st.globalRepo = &policy
	if err := st.Open(ctx); err != nil {
		return nil, fmt.Errorf("open global replica (stop its current operation/owner before retrying; no operation was submitted): %w", err)
	}
	version, err := st.SchemaVersion(ctx)
	if err == nil && version != store.LatestSchemaVersion() {
		err = errors.New("global replica requires explicit `memdolt global init` for its older schema")
	}
	if err != nil {
		return nil, errors.Join(err, st.Close())
	}
	return st, nil
}

func (s *Store) policyPaths() layout.Paths {
	if s.globalRepo != nil {
		return *s.globalRepo
	}
	return s.paths
}

func (s *Store) globalWriteActor(actor memory.Actor) error {
	if s.globalRepo == nil {
		return nil
	}
	if err := requireGlobalEnabled(s.globalRepo.Base()); err != nil {
		return err
	}
	normalized, err := memory.NormalizeActor(actor.Raw)
	if err != nil || actor.Name != memory.UserActor.Name || normalized.Name != actor.Name {
		return errors.New("global memory writes require the trusted human user actor; global proposal acceptance remains a separate terminal workflow")
	}
	return nil
}
