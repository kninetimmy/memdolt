package localdolt

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/BurntSushi/toml"

	"github.com/kninetimmy/memdolt/internal/layout"
)

type documentConfig struct {
	Doc struct {
		AllowedDirs []string `toml:"allowed_dirs"`
	} `toml:"doc"`
	Retrieval struct {
		IncludeDocsInDefault bool `toml:"include_docs_in_default"`
	} `toml:"retrieval"`
}

// A missing optional config uses defaults. An existing link, unreadable file,
// malformed TOML or invalid doc table never becomes an empty allow-list.
func readDocumentConfig(root *os.Root) (cfg documentConfig, raw []byte, mode os.FileMode, err error) {
	info, err := root.Lstat(layout.ConfigFileName)
	if os.IsNotExist(err) {
		return cfg, nil, 0o600, nil
	}
	if err != nil {
		return cfg, nil, 0, fmt.Errorf("inspect document configuration: %w", err)
	}
	if !info.Mode().IsRegular() {
		return cfg, nil, 0, errors.New("document configuration must be a regular file, not a link or directory")
	}
	raw, err = root.ReadFile(layout.ConfigFileName)
	if err != nil {
		return cfg, nil, 0, fmt.Errorf("read document configuration: %w", err)
	}
	metadata, err := toml.Decode(string(raw), &cfg)
	if err != nil {
		return cfg, nil, 0, fmt.Errorf("parse document configuration: %w", err)
	}
	for _, key := range metadata.Undecoded() {
		if len(key) > 0 && key[0] == "doc" {
			return cfg, nil, 0, errors.New("unknown [doc] configuration key; use allowed_dirs")
		}
	}
	return cfg, raw, info.Mode().Perm(), nil
}

func (s *Store) documentConfigRoot() (*os.Root, error) {
	base, err := filepath.EvalSymlinks(s.paths.Base())
	if err != nil {
		return nil, fmt.Errorf("resolve document repository root: %w", err)
	}
	dir := filepath.Join(base, layout.DirName)
	if err := clonePath(base, dir); err != nil {
		return nil, err
	}
	return os.OpenRoot(dir)
}

// Only first-document ingestion calls this, after its Dolt commit. Decode the
// latest complete config into a map so unknown tables survive semantically.
// A temporary file and rooted rename avoid truncating the operator's config.
func enableDocumentRecall(root *os.Root) (enabled bool, err error) {
	cfg, raw, mode, err := readDocumentConfig(root)
	if err != nil || cfg.Retrieval.IncludeDocsInDefault {
		return false, err
	}
	values := map[string]any{}
	if _, err := toml.Decode(string(raw), &values); err != nil {
		return false, fmt.Errorf("parse configuration for document recall: %w", err)
	}
	retrieval, ok := values["retrieval"].(map[string]any)
	if !ok {
		retrieval = map[string]any{}
		values["retrieval"] = retrieval
	}
	retrieval["include_docs_in_default"] = true
	var encoded bytes.Buffer
	if err := toml.NewEncoder(&encoded).Encode(values); err != nil {
		return false, fmt.Errorf("encode document recall configuration: %w", err)
	}
	name := ".doc-config-" + newID() + ".tmp"
	file, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return false, fmt.Errorf("prepare document recall configuration: %w", err)
	}
	defer func() {
		if removeErr := root.Remove(name); removeErr != nil && !os.IsNotExist(removeErr) {
			err = errors.Join(err, fmt.Errorf("remove temporary document config %s: %w", name, removeErr))
		}
	}()
	_, writeErr := file.Write(encoded.Bytes())
	err = errors.Join(writeErr, file.Sync(), file.Close())
	if err != nil {
		return false, fmt.Errorf("write document recall configuration: %w", err)
	}
	// Refuse a detected concurrent edit. This is not a lock on foreign config
	// editors in the final read/rename interval; cooperating ingests share Store's
	// mutation mutex through finalization.
	_, current, _, err := readDocumentConfig(root)
	if err != nil {
		return false, err
	}
	if !bytes.Equal(current, raw) {
		return false, errors.New("document configuration changed during finalization")
	}
	if err := root.Rename(name, layout.ConfigFileName); err != nil {
		return false, fmt.Errorf("replace document recall configuration: %w", err)
	}
	return true, nil
}

func documentString(label, value string, maxChars int) error {
	if !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
		return fmt.Errorf("document %s must be valid UTF-8 without NUL", label)
	}
	if maxChars > 0 && utf8.RuneCountInString(value) > maxChars {
		return fmt.Errorf("document %s exceeds %d characters", label, maxChars)
	}
	return nil
}

func documentPathArgument(path string) error {
	if err := documentString("path or id", path, 1024); err != nil {
		return err
	}
	if strings.TrimSpace(path) == "" || strings.ContainsAny(path, "\r\n") {
		return errors.New("document path or id must be nonempty and contain no line breaks")
	}
	return nil
}

func (s *Store) readDocumentFile(opts DocAddOptions, cfg documentConfig) (path string, data []byte, err error) {
	if err := documentPathArgument(opts.File); err != nil {
		return "", nil, err
	}
	if err := documentString("title", opts.Title, 512); err != nil {
		return "", nil, err
	}
	file := opts.File
	if !filepath.IsAbs(file) {
		file = filepath.Join(s.paths.Base(), file)
	}
	path, err = filepath.EvalSymlinks(file)
	if err != nil {
		return "", nil, fmt.Errorf("resolve document file: %w", err)
	}
	if err := documentPathArgument(path); err != nil {
		return "", nil, err
	}
	rootPath := filepath.Dir(path)
	if opts.Confined {
		base, err := filepath.EvalSymlinks(s.paths.Base())
		if err != nil {
			return "", nil, fmt.Errorf("resolve document repository root: %w", err)
		}
		rootPath = ""
		for _, candidate := range append([]string{base}, cfg.Doc.AllowedDirs...) {
			if candidate == "" {
				continue
			}
			if !filepath.IsAbs(candidate) {
				candidate = filepath.Join(base, candidate)
			}
			canonical, err := filepath.EvalSymlinks(candidate)
			if err != nil {
				continue // An unresolved entry grants no access, as in memhub.
			}
			rel, err := filepath.Rel(canonical, path)
			if err == nil && filepath.IsLocal(rel) {
				rootPath = canonical
				break
			}
		}
		if rootPath == "" {
			return "", nil, errors.New("doc_add file is outside the repository root and [doc] allowed_dirs")
		}
	}
	if err := s.checkDenyList([]string{opts.File, path, opts.Title, opts.Actor.Name, opts.Actor.Raw}); err != nil {
		return "", nil, err
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return "", nil, fmt.Errorf("open document root: %w", err)
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	rel, err := filepath.Rel(rootPath, path)
	if err != nil || !filepath.IsLocal(rel) {
		return "", nil, errors.New("document file escapes its selected root")
	}
	info, err := root.Stat(rel)
	if err != nil {
		return "", nil, fmt.Errorf("inspect document file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", nil, errors.New("document source must be a regular file")
	}
	opened, err := root.Open(rel)
	if err != nil {
		return "", nil, fmt.Errorf("open document file: %w", err)
	}
	defer func() { err = errors.Join(err, opened.Close()) }()
	info, err = opened.Stat()
	if err != nil {
		return "", nil, fmt.Errorf("inspect opened document: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", nil, errors.New("opened document source must be a regular file")
	}
	data, err = io.ReadAll(opened)
	if err != nil {
		return "", nil, fmt.Errorf("read document file: %w", err)
	}
	if !utf8.Valid(data) {
		return "", nil, errors.New("document content must be valid UTF-8")
	}
	return path, data, nil
}

// Exact paths still resolve after their source file has been removed. CLI
// callers resolve relative paths in their own process before owner submission.
func (s *Store) documentIdentityPath(ident string) string {
	path := ident
	if !filepath.IsAbs(path) {
		path = filepath.Join(s.paths.Base(), path)
	}
	if canonical, err := filepath.EvalSymlinks(path); err == nil {
		return canonical
	}
	return filepath.Clean(path)
}
