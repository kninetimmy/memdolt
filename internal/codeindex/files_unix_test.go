//go:build linux || darwin

package codeindex

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestUnreadableUnixSourceDropsPriorChunks(t *testing.T) {
	ctx := context.Background()
	root := testRepo(t, map[string]string{"a.go": "package a\nfunc Canary() {}"})
	if _, err := Refresh(ctx, root, nil); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "a.go")
	if err := os.Chmod(file, 0); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chmod(file, 0o600); err != nil {
			t.Error(err)
		}
	}()
	if f, err := os.Open(file); err == nil {
		_ = f.Close()
		t.Skip("current user can read chmod-000 files")
	}
	if _, err := Locate(ctx, root, nil, Options{Query: "Canary", NoRefresh: true}); err == nil {
		t.Fatal("unreadable source silently returned")
	}
	summary, err := Refresh(ctx, root, nil)
	if err != nil || summary.SkippedFiles != 1 || summary.DeletedFiles != 1 || summary.ChunksTotal != 0 {
		t.Fatalf("unreadable source retained stale data: %+v %v", summary, err)
	}
}
