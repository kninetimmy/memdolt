//go:build !windows

package main

import (
	"os"
	"testing"
)

func makeBodyFileUnreadable(t *testing.T, file string) {
	t.Helper()
	if err := os.Chmod(file, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(file, 0o600); err != nil {
			t.Error(err)
		}
	})
	if f, err := os.Open(file); err == nil {
		_ = f.Close()
		t.Skip("current user can read chmod-000 sources")
	}
}
