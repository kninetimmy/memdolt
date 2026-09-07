package codeindex

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestUnreadableWindowsSourceDropsPriorChunks(t *testing.T) {
	ctx := context.Background()
	root := testRepo(t, map[string]string{"a.go": "package a\nfunc Canary() {}"})
	if _, err := Refresh(ctx, root, nil); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(filepath.Join(root, "a.go"), stamp, stamp); err != nil {
		t.Fatal(err)
	}
	name, err := syscall.UTF16PtrFromString(filepath.Join(root, "a.go"))
	if err != nil {
		t.Fatal(err)
	}
	handle, err := syscall.CreateFile(name, syscall.GENERIC_READ, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := syscall.CloseHandle(handle); err != nil {
			t.Error(err)
		}
	}()
	// A mandatory byte-range lock exercises a real ReadFile failure, including
	// filesystems where share-mode restrictions are not enforced on openat.
	lock := syscall.NewLazyDLL("kernel32.dll").NewProc("LockFile")
	if err := lock.Find(); err != nil {
		t.Fatal(err)
	}
	if ok, _, err := lock.Call(uintptr(handle), 0, 0, 1024, 0); ok == 0 {
		t.Fatal(err)
	}
	if result, err := Locate(ctx, root, nil, Options{Query: "Canary", NoRefresh: true}); err == nil {
		t.Fatalf("unreadable source silently returned: %+v", result)
	}
	summary, err := Refresh(ctx, root, nil)
	if err != nil || summary.SkippedFiles != 1 || summary.DeletedFiles != 1 || summary.ChunksTotal != 0 {
		t.Fatalf("unreadable source retained stale data: %+v %v", summary, err)
	}
}
