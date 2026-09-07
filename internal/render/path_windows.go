package render

import (
	"os"
	"syscall"
)

func unsafeLink(info os.FileInfo) bool {
	data, ok := info.Sys().(*syscall.Win32FileAttributeData)
	return !ok || data.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0
}
