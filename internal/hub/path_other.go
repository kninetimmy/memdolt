//go:build !windows

package hub

import (
	"errors"
	"os"
	"syscall"
)

func unsafeLink(info os.FileInfo) bool { return info.Mode()&os.ModeSymlink != 0 }

func singleLink(f *os.File) (bool, error) {
	info, err := f.Stat()
	if err != nil {
		return false, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false, errors.New("cannot verify hub file link count")
	}
	return stat.Nlink == 1, nil
}

func rootOwned(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == 0 && info.Mode().Perm()&0o022 == 0
}
