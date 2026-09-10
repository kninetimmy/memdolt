package main

import (
	"syscall"
	"testing"
)

func makeBodyFileUnreadable(t *testing.T, file string) {
	t.Helper()
	name, err := syscall.UTF16PtrFromString(file)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := syscall.CreateFile(name, syscall.GENERIC_READ, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := syscall.CloseHandle(handle); err != nil {
			t.Error(err)
		}
	})
	// Mandatory byte-range locking reaches a real ReadFile error even where
	// rooted opens do not enforce ordinary Windows share-mode restrictions.
	lock := syscall.NewLazyDLL("kernel32.dll").NewProc("LockFile")
	if err := lock.Find(); err != nil {
		t.Fatal(err)
	}
	if ok, _, err := lock.Call(uintptr(handle), 0, 0, 1024, 0); ok == 0 {
		t.Fatal(err)
	}
}
