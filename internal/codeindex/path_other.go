//go:build !windows

package codeindex

import (
	"fmt"
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
		return false, fmt.Errorf("cannot verify code-index file link count")
	}
	return stat.Nlink == 1, nil
}
