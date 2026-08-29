// Package layout resolves memdolt's per-repository on-disk paths.
//
// It exists so that the packages which share those paths — the store
// (PRD §5.1/§5.2) and the single-owner IPC endpoint (PRD §5.2) — agree on
// them without depending on each other. Every path is derived from a
// caller-supplied base directory; nothing here consults the working
// directory or hardcodes a location, so tests and the M0 rig can point a
// store at a temporary directory.
//
// The layout follows PRD §5.3:
//
//	<base>/.memdolt/          memdolt's per-repository state
//	  dolt/                   Dolt data dir; database name = "memory"
//	  config.toml             per-machine configuration (§11.3)
//	  embeddings.sqlite       derived embedding side-store (§8.2)
//	  LOCK                    memdolt's single-owner store lock (§5.2)
//	  server.pid              MCP server pidfile (§5.2)
package layout

import (
	"errors"
	"fmt"
	"path/filepath"
)

const (
	// DirName is the per-repository directory memdolt keeps its state in.
	DirName = ".memdolt"

	// DoltDataDirName is the Dolt data directory beneath DirName. The
	// embedded driver treats each of its subdirectories as one database.
	DoltDataDirName = "dolt"

	// LockFileName is memdolt's own single-owner lock file (PRD §5.2).
	//
	// It deliberately sits beside the Dolt data directory rather than
	// inside it, for two reasons: the embedded driver scans the data
	// directory for databases and should never have to reason about a
	// stray file, and Dolt keeps an unrelated internal LOCK file of its
	// own at <data dir>/<database>/.dolt/noms/LOCK that this one must not
	// be confused with.
	LockFileName = "LOCK"

	// PidFileName is the single-owner pidfile written by the IPC endpoint
	// (PRD §5.2, §5.3). It holds a live credential and is created 0600.
	PidFileName = "server.pid"

	// ConfigFileName is the per-repository configuration file (PRD §5.3,
	// §11.3). It is per-machine and gitignored; a repository checks in
	// config.example.toml instead.
	//
	// Its absence is not an error anywhere: every key it holds has a
	// default, and a repository with no config file is configured with
	// those defaults.
	ConfigFileName = "config.toml"

	// EmbeddingsFileName is the derived, machine-local embedding side-store
	// (PRD §5.3, §8.2). It never belongs in the Dolt data directory.
	EmbeddingsFileName = "embeddings.sqlite"
)

// ErrNoBaseDir is returned by New when it is given an empty base directory.
var ErrNoBaseDir = errors.New("layout: base directory is required")

// Paths locates memdolt's files beneath one repository's base directory.
// The zero value is not usable; construct it with New.
type Paths struct {
	base string
}

// New resolves base to an absolute path and returns the layout rooted at
// it. base is normally a git repository root, but nothing here requires
// that; it only has to be a directory memdolt may write beneath. New does
// not create anything on disk.
func New(base string) (Paths, error) {
	if base == "" {
		return Paths{}, ErrNoBaseDir
	}
	abs, err := filepath.Abs(base)
	if err != nil {
		return Paths{}, fmt.Errorf("layout: resolve base directory %q: %w", base, err)
	}
	return Paths{base: abs}, nil
}

// Base returns the absolute base directory.
func (p Paths) Base() string { return p.base }

// Dir returns <base>/.memdolt.
func (p Paths) Dir() string { return filepath.Join(p.base, DirName) }

// DoltDataDir returns <base>/.memdolt/dolt, the directory the embedded
// Dolt driver is rooted at.
func (p Paths) DoltDataDir() string { return filepath.Join(p.Dir(), DoltDataDirName) }

// LockFile returns <base>/.memdolt/LOCK, memdolt's single-owner store lock.
func (p Paths) LockFile() string { return filepath.Join(p.Dir(), LockFileName) }

// PidFile returns <base>/.memdolt/server.pid, the IPC endpoint's pidfile.
func (p Paths) PidFile() string { return filepath.Join(p.Dir(), PidFileName) }

// ConfigFile returns <base>/.memdolt/config.toml, this repository's
// per-machine configuration (PRD §11.3).
func (p Paths) ConfigFile() string { return filepath.Join(p.Dir(), ConfigFileName) }

// EmbeddingsFile returns <base>/.memdolt/embeddings.sqlite, the derived
// embedding side-store that is deliberately outside Dolt history (PRD §8.2).
func (p Paths) EmbeddingsFile() string { return filepath.Join(p.Dir(), EmbeddingsFileName) }
