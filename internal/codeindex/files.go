package codeindex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/kninetimmy/memdolt/internal/layout"
)

const (
	indexName = "code_index.sqlite"
	lockName  = "code_index.lock"
)

var errUnsafePath = errors.New("unsafe code-index path")
var ErrBusy = errors.New("code index is busy; after a crash, stop code operations and inspect code_index.lock before removing that lock")

type protectionError struct{ error }

type repository struct {
	base           string
	root, metadata *os.Root
	lock           *os.File
	lockInfo       os.FileInfo
	cfg            configuration
}

// ResolveRoot finds the nearest git root without invoking git. No-refresh can
// therefore select an existing index from a subdirectory without a git walk.
func ResolveRoot(start string) (string, error) {
	abs, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(filepath.ToSlash(abs), "//") {
		return "", errors.New("code repository must use a local filesystem path")
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve code repository: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		return "", errors.Join(errors.New("code repository must be a directory"), err)
	}
	for dir := abs; ; dir = filepath.Dir(dir) {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return dir, nil
		} else if !os.IsNotExist(err) {
			return "", fmt.Errorf("inspect code repository: %w", err)
		}
		if filepath.Dir(dir) == dir {
			break
		}
	}
	return abs, nil // Status can report an unindexed directory without creating it.
}

func openRepository(start string, write bool) (_ *repository, err error) {
	base, err := ResolveRoot(start)
	if err != nil {
		return nil, err
	}
	r := &repository{base: base}
	defer func() {
		if err != nil {
			err = errors.Join(err, r.close())
		}
	}()
	r.root, err = os.OpenRoot(base)
	if err != nil {
		return nil, err
	}
	info, err := r.root.Lstat(layout.DirName)
	if os.IsNotExist(err) && write {
		if err := r.root.Mkdir(layout.DirName, 0o700); err != nil && !os.IsExist(err) {
			return nil, fmt.Errorf("create code-index directory: %w", err)
		}
		info, err = r.root.Lstat(layout.DirName)
	}
	if err == nil {
		if unsafeLink(info) || !info.IsDir() {
			return nil, fmt.Errorf("%w: metadata must be an unlinked directory", errUnsafePath)
		}
		r.metadata, err = r.root.OpenRoot(layout.DirName)
		if err != nil {
			return nil, err
		}
		if err := r.verifyMetadata(); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	r.cfg, err = readConfig(r.metadata)
	if err != nil {
		return nil, err
	}
	return r, nil
}

func (r *repository) verifyMetadata() error {
	baseInfo, err := os.Lstat(r.base)
	if err != nil {
		return err
	}
	rootInfo, err := r.root.Stat(".")
	if err != nil {
		return err
	}
	if unsafeLink(baseInfo) || !os.SameFile(baseInfo, rootInfo) {
		return fmt.Errorf("%w: repository root changed", errUnsafePath)
	}
	info, err := r.root.Lstat(layout.DirName)
	if err != nil {
		return err
	}
	actual, err := r.metadata.Stat(".")
	if err != nil {
		return err
	}
	if unsafeLink(info) || !os.SameFile(info, actual) {
		return fmt.Errorf("%w: metadata directory changed", errUnsafePath)
	}
	return nil
}

// An exclusively created lock covers the complete refresh/query/remove, not
// Dolt operations. Like render's lock, crash residue requires inspection.
// ponytail: one operation per repository; split inference from this lock only
// if measured contention warrants a protocol preserving query/remove safety.
func (r *repository) acquire() error {
	if r.metadata == nil {
		return errors.New("code index is missing; run `memdolt code index`")
	}
	if err := r.verifyMetadata(); err != nil {
		return err
	}
	f, err := r.metadata.OpenFile(lockName, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if os.IsExist(err) {
		return ErrBusy
	}
	if err != nil {
		return fmt.Errorf("lock code index: %w", err)
	}
	r.lock = f
	r.lockInfo, err = f.Stat()
	return err
}

func (r *repository) close() error {
	var err error
	if r.lock != nil {
		info, checkErr := r.metadata.Lstat(lockName)
		if checkErr == nil && (r.lockInfo == nil || unsafeLink(info) || !os.SameFile(info, r.lockInfo)) {
			checkErr = errors.New("code index lock identity changed; foreign file retained")
		}
		err = errors.Join(err, checkErr, r.lock.Close())
		if checkErr == nil {
			err = errors.Join(err, r.metadata.Remove(lockName))
		}
	}
	if r.metadata != nil {
		err = errors.Join(err, r.metadata.Close())
	}
	if r.root != nil {
		err = errors.Join(err, r.root.Close())
	}
	return err
}

func validateSourcePath(file string) error {
	if !utf8.ValidString(file) || strings.ContainsAny(file, "\\:\x00") || !fs.ValidPath(file) || file == "." || !filepath.IsLocal(filepath.FromSlash(file)) {
		return errUnsafePath
	}
	for _, part := range strings.Split(file, "/") {
		if strings.EqualFold(part, ".git") || strings.EqualFold(part, ".memdolt") || strings.EqualFold(part, ".memhub") || strings.EqualFold(part, ".orchestrator") {
			return errUnsafePath
		}
	}
	return nil
}

// Check every component before the rooted open. os.Root keeps a replacement
// link from escaping the root; the opened identity is checked before reads.
func openRegular(root *os.Root, file string) (_ *os.File, _ os.FileInfo, err error) {
	parts := strings.Split(file, "/")
	var expected os.FileInfo
	for i := range parts {
		expected, err = root.Lstat(filepath.FromSlash(strings.Join(parts[:i+1], "/")))
		if err != nil {
			return nil, nil, err
		}
		if unsafeLink(expected) || i < len(parts)-1 && !expected.IsDir() {
			return nil, nil, errUnsafePath
		}
	}
	if !expected.Mode().IsRegular() {
		return nil, nil, errUnsafePath
	}
	f, err := root.Open(filepath.FromSlash(file))
	if err != nil {
		return nil, nil, err
	}
	actual, err := f.Stat()
	if err == nil && (!actual.Mode().IsRegular() || !os.SameFile(expected, actual)) {
		err = errUnsafePath
	}
	if err != nil {
		if closeErr := f.Close(); closeErr != nil {
			return nil, nil, &protectionError{errors.Join(err, closeErr)}
		}
		return nil, nil, err
	}
	return f, actual, nil
}

func (r *repository) openSource(file string) (_ *os.File, _ os.FileInfo, err error) {
	if err := validateSourcePath(file); err != nil {
		return nil, nil, err
	}
	f, info, err := openRegular(r.root, file)
	if err != nil {
		return nil, nil, err
	}
	if r.metadata != nil {
		if err := r.verifyMetadata(); err != nil {
			return nil, nil, &protectionError{errors.Join(err, f.Close())}
		}
	}
	if err := layout.CheckOwnerSource(r.metadata, info); err != nil {
		if closeErr := f.Close(); closeErr != nil {
			return nil, nil, &protectionError{errors.Join(err, closeErr)}
		}
		if errors.Is(err, layout.ErrOwnerSource) {
			return nil, nil, err
		}
		return nil, nil, &protectionError{err}
	}
	return f, info, nil
}

func readSource(f *os.File, before os.FileInfo) ([]byte, error) {
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || int64(len(data)) != after.Size() {
		return nil, errors.New("code source changed while being read")
	}
	return data, nil
}

func trackedFiles(ctx context.Context, base string) ([]string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", base, "ls-files", "-z").Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files for code index: %w", err)
	}
	if !utf8.Valid(out) {
		return nil, errors.New("git tracked paths must be UTF-8")
	}
	files := strings.Split(string(out), "\x00")
	files = slices.DeleteFunc(files, func(s string) bool { return s == "" })
	slices.Sort(files)
	return slices.Compact(files), nil
}

func currentHead(ctx context.Context, base string) *string {
	out, err := exec.CommandContext(ctx, "git", "-C", base, "rev-parse", "--verify", "HEAD").Output()
	if err != nil {
		return nil // An unborn or non-git directory has no reporting hash.
	}
	head := strings.TrimSpace(string(out))
	if head == "" {
		return nil
	}
	return &head
}
