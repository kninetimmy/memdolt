package localdolt

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/dolthub/dolt/go/libraries/utils/filesys"
)

// cloneFS bounds the environment's writes and refuses its cleanup and moves.
// In particular InitRepoWithNoData's bestEffortDeleteAll and LoadDoltDB's
// asynchronous old-temp-file sweep must not remove foreign content. The NBS
// engine still manages its own newly-created table files inside memory/.dolt.
type cloneFS struct {
	filesys.Filesys
	root string
}

func newCloneFS(root string) (*cloneFS, error) {
	fs, err := filesys.LocalFilesysWithWorkingDir(root)
	if err != nil {
		return nil, err
	}
	return &cloneFS{Filesys: fs, root: root}, nil
}

// clonePath rejects links rather than following them into another destination.
// The selected repository itself has already been resolved by EvalSymlinks.
func clonePath(root, path string) error {
	rel, err := filepath.Rel(root, path)
	if err != nil || !filepath.IsLocal(rel) {
		return errors.New("clone path escapes the selected destination")
	}
	for path != root {
		info, err := os.Lstat(path)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("inspect clone path: %w", err)
		}
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("clone refuses symbolic links in its managed paths: %s", path)
		}
		path = filepath.Dir(path)
	}
	return nil
}

func (f *cloneFS) Abs(path string) (string, error) {
	abs, err := f.Filesys.Abs(path)
	if err != nil {
		return "", err
	}
	if err := clonePath(f.root, filepath.Clean(abs)); err != nil {
		return "", err
	}
	return abs, nil
}

func (f *cloneFS) WithWorkingDir(path string) (filesys.Filesys, error) {
	abs, err := f.Abs(path)
	if err != nil {
		return nil, err
	}
	fs, err := f.Filesys.WithWorkingDir(abs)
	if err != nil {
		return nil, err
	}
	return &cloneFS{Filesys: fs, root: f.root}, nil
}

func (f *cloneFS) MkDirs(path string) error {
	abs, err := f.Abs(path)
	if err != nil {
		return err
	}
	return os.MkdirAll(abs, 0o700)
}

func (f *cloneFS) WriteFile(path string, data []byte, mode os.FileMode) error {
	abs, err := f.Abs(path)
	if err != nil {
		return err
	}
	return f.Filesys.WriteFile(abs, data, mode)
}

func (f *cloneFS) OpenForWrite(path string, mode os.FileMode) (io.WriteCloser, error) {
	abs, err := f.Abs(path)
	if err != nil {
		return nil, err
	}
	return f.Filesys.OpenForWrite(abs, mode)
}

func (f *cloneFS) OpenForWriteAppend(path string, mode os.FileMode) (io.WriteCloser, error) {
	abs, err := f.Abs(path)
	if err != nil {
		return nil, err
	}
	return f.Filesys.OpenForWriteAppend(abs, mode)
}

func (*cloneFS) Delete(string, bool) error {
	return errors.New("clone retains artifacts; automatic deletion is disabled")
}
func (*cloneFS) DeleteFile(string) error {
	return errors.New("clone retains artifacts; automatic deletion is disabled")
}
func (*cloneFS) MoveFile(string, string) error {
	return errors.New("clone does not move existing files")
}
func (*cloneFS) MoveDir(string, string) error {
	return errors.New("clone does not move existing directories")
}

// Dolt's clone flow uses TempTableFilesDir; keep any fallback inside the root too.
func (f *cloneFS) TempDir() string { return filepath.Join(f.root, DatabaseName, ".dolt", "tmp") }
