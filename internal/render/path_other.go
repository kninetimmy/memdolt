//go:build !windows

package render

import "os"

func unsafeLink(info os.FileInfo) bool { return info.Mode()&os.ModeSymlink != 0 }
