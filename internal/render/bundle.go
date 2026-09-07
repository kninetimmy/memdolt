package render

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// BundlePath applies the renderer's rooted local-file discipline to an
// explicitly selected interop file. No bundle may live in repository plumbing.
// Unlike renderer configuration, relative CLI operands are resolved by the CLI.
func BundlePath(path string) error {
	if !filepath.IsAbs(path) || strings.HasPrefix(filepath.VolumeName(path), `\\`) {
		return errors.New("bundle path must be an absolute local filesystem path")
	}
	for _, part := range strings.FieldsFunc(path[len(filepath.VolumeName(path)):], func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == "." || part == ".." || runtime.GOOS == "windows" && (strings.ContainsAny(part, `:<>"|?*`) || strings.TrimRight(part, " .") != part) {
			return errors.New("unsafe bundle path component")
		}
		switch strings.ToLower(part) {
		case ".git", ".memdolt", ".memhub", ".orchestrator":
			return errors.New("bundle path collides with protected store, configuration, side-store or credentials; choose a file outside repository metadata")
		}
	}
	return nil
}

// ReadBundle verifies the actual opened file before reading any bytes. The
// store's check protects opened owner credentials, including hard-link aliases.
func ReadBundle(path string, check func(os.FileInfo) error) (data []byte, err error) {
	if err := BundlePath(path); err != nil {
		return nil, err
	}
	root, err := openDir(filepath.Dir(path), false)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	return readBundleFile(root, filepath.Base(path), check)
}

func readBundleFile(root *os.Root, name string, check func(os.FileInfo) error) (data []byte, err error) {
	expected, err := regular(root, name)
	if err != nil || expected == nil {
		return nil, errors.Join(errors.New("bundle requires an existing regular file"), err)
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	actual, err := file.Stat()
	if err != nil || !os.SameFile(expected, actual) || !actual.Mode().IsRegular() {
		return nil, errors.Join(errors.New("bundle file changed while opening"), err)
	}
	if err := check(actual); err != nil {
		return nil, err
	}
	return io.ReadAll(file)
}

// PublishBundle prepares and syncs a complete sibling file before rooted native
// replacement. It reports confirmed replacement even if final cleanup fails.
// Existing outputs must be recognizable bundles, never arbitrary source files.
func PublishBundle(ctx context.Context, path string, data []byte, check func(os.FileInfo) error, validatePrevious func([]byte) error) (bool, error) {
	return publishBundle(ctx, path, data, check, validatePrevious, fileHooks{})
}

func publishBundle(ctx context.Context, path string, data []byte, check func(os.FileInfo) error, validatePrevious func([]byte) error, hooks fileHooks) (written bool, err error) {
	if err := BundlePath(path); err != nil {
		return false, err
	}
	// The explicitly selected parent must already exist; exporting creates no
	// directory trees and has no cleanup authority over operator directories.
	root, err := openDir(filepath.Dir(path), false)
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	name := filepath.Base(path)
	lockName := "." + name + ".memdolt-export.lock"
	lock, err := root.OpenFile(lockName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return false, fmt.Errorf("export destination is locked or unavailable; after a crash stop exports and inspect output before removing %s: %w", lockName, err)
	}
	lockInfo, statErr := lock.Stat()
	if err := errors.Join(statErr, lock.Close()); err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, removeOwned(root, lockName, lockInfo)) }()
	original, err := regular(root, name)
	if err != nil {
		return false, err
	}
	var previous []byte
	if original != nil {
		previous, err = readBundleFile(root, name, check)
		if err != nil {
			return false, err
		}
		if err := validatePrevious(previous); err != nil {
			return false, fmt.Errorf("refuse to replace an unrecognized output/source file; choose an unused export destination: %w", err)
		}
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if hooks.beforePrepare != nil {
		if err := hooks.beforePrepare(0); err != nil {
			return false, err
		}
	}
	temp, info, err := createFile(root, "."+name+".tmp-", data)
	if err != nil {
		return false, err
	}
	defer func() {
		if temp != "" {
			err = errors.Join(err, removeOwned(root, temp, info))
		}
	}()
	if hooks.beforeReplace != nil {
		if err := hooks.beforeReplace(0); err != nil {
			return false, err
		}
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	current, err := regular(root, name)
	if err != nil || (current == nil) != (original == nil) || current != nil && !os.SameFile(current, original) {
		return false, errors.Join(errors.New("export destination changed during preparation"), err)
	}
	if current != nil {
		actual, err := readBundleFile(root, name, check)
		if err != nil || string(actual) != string(previous) {
			return false, errors.Join(errors.New("export destination contents changed during preparation"), err)
		}
	}
	if err := root.Rename(temp, name); err != nil {
		return false, fmt.Errorf("replace export file: %w", err)
	}
	temp, written = "", true
	if hooks.finalize != nil {
		return written, hooks.finalize()
	}
	return written, nil
}
