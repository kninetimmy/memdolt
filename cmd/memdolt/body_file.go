package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/kninetimmy/memdolt/internal/layout"
)

const bodyFileLimit = 65535 // Native TEXT bytes, before the writer's TrimSpace.

func bindBodyFile(cmd *cobra.Command) {
	cmd.Flags().String("from-file", "", "read a UTF-8 body file relative to the process directory (independent of --dir)")
	cmd.Long += "\n\nA text argument and --from-file are mutually exclusive, even when empty. With\n" +
		"neither, read standard input. Files must be regular, nonblank UTF-8 and at most\n" +
		"65535 bytes before whitespace normalization. Relative paths use the process\n" +
		"directory; --dir selects the repository whose owner credential and aliases\n" +
		"are protected. File validation and close finish before opening memory.\n" +
		"Explicit text files outside that repository and rendered narratives are allowed."
}

// Only the four CLI body-file routes use this reader. Canonical selection and
// an opened-file comparison allow explicit external files without inheriting
// document ingestion's configuration or code indexing's metadata exclusions.
func readBodyFile(dir, path string) (body string, err error) {
	defer func() {
		if err != nil {
			body = ""
			err = fmt.Errorf("read --from-file body: %w", err)
		}
	}()
	if path == "" {
		return "", errors.New("--from-file requires a nonempty file path")
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", err
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve source: %w", err)
	}
	expected, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !expected.Mode().IsRegular() {
		return "", errors.New("source must be a regular file")
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return "", fmt.Errorf("open source directory: %w", err)
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	file, err := root.Open(filepath.Base(path))
	if err != nil {
		return "", fmt.Errorf("open source: %w", err)
	}
	actual, err := file.Stat()
	if err == nil && (!actual.Mode().IsRegular() || !os.SameFile(expected, actual)) {
		err = errors.New("source identity changed while opening")
	}
	if err == nil {
		err = checkBodyFileOwner(dir, actual)
	}
	if err != nil {
		return "", errors.Join(err, file.Close())
	}
	return readBodyFileContent(file)
}

func readBodyFileContent(file io.ReadCloser) (string, error) {
	data, err := io.ReadAll(io.LimitReader(file, bodyFileLimit+1))
	if err := errors.Join(err, file.Close()); err != nil {
		return "", fmt.Errorf("read/close source: %w", err)
	}
	if len(data) > bodyFileLimit {
		return "", errors.New("source exceeds the 65535-byte TEXT limit")
	}
	if !utf8.Valid(data) {
		return "", errors.New("source content must be valid UTF-8")
	}
	body := string(data)
	if strings.TrimSpace(body) == "" {
		return "", errors.New("source body must not be empty or whitespace-only")
	}
	return body, nil // The existing memory writer still owns normalization.
}

// Inspect only the selected repository's existing metadata. No store or config
// is opened or created. A missing metadata directory has no known owner file;
// any failure to establish its identity refuses the source before content read.
func checkBodyFileOwner(dir string, source os.FileInfo) (err error) {
	base, err := filepath.Abs(dir)
	if err == nil {
		base, err = filepath.EvalSymlinks(base)
	}
	if err != nil {
		return fmt.Errorf("resolve repository for owner-file protection: %w", err)
	}
	expected, err := os.Lstat(base)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	actual, err := root.Stat(".")
	if err != nil || !os.SameFile(expected, actual) {
		return errors.Join(errors.New("repository identity changed during owner-file protection"), err)
	}
	expected, err = root.Lstat(layout.DirName)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !expected.IsDir() || expected.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return errors.New("cannot verify protected memdolt owner metadata: metadata must be an unlinked directory")
	}
	metadata, err := root.OpenRoot(layout.DirName)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, metadata.Close()) }()
	actual, err = metadata.Stat(".")
	if err != nil || !os.SameFile(expected, actual) {
		return errors.Join(errors.New("metadata directory identity changed during owner-file protection"), err)
	}
	return layout.CheckOwnerSource(metadata, source)
}
