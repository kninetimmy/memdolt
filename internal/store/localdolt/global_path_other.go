//go:build !windows

package localdolt

import "os"

func globalUnsafeLink(info os.FileInfo) bool { return info.Mode()&os.ModeSymlink != 0 }
