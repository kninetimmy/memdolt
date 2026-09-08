package hub

import (
	"os"
	"syscall"
)

func unsafeLink(info os.FileInfo) bool {
	data, ok := info.Sys().(*syscall.Win32FileAttributeData)
	return !ok || data.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0
}

func singleLink(f *os.File) (bool, error) {
	var info syscall.ByHandleFileInformation
	err := syscall.GetFileInformationByHandle(syscall.Handle(f.Fd()), &info)
	return info.NumberOfLinks == 1, err
}

func rootOwned(os.FileInfo) bool { return false }
