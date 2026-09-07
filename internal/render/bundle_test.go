package render

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestBundlePublicationPreservesExistingOutputOnFailures(t *testing.T) {
	for _, phase := range []string{"prepare", "replace", "finalize", "changed"} {
		t.Run(phase, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "bundle.json")
			if err := os.WriteFile(path, []byte("original complete bundle"), 0o600); err != nil {
				t.Fatal(err)
			}
			failure := errors.New("synthetic publication failure")
			hooks := fileHooks{}
			switch phase {
			case "prepare":
				hooks.beforePrepare = func(int) error { return failure }
			case "replace":
				hooks.beforeReplace = func(int) error { return failure }
			case "finalize":
				hooks.finalize = func() error { return failure }
			case "changed":
				hooks.beforeReplace = func(int) error { return os.WriteFile(path, []byte("foreign writer changed output"), 0o600) }
			}
			written, err := publishBundle(context.Background(), path, []byte("new complete bundle"), func(os.FileInfo) error { return nil }, func([]byte) error { return nil }, hooks)
			if err == nil || written != (phase == "finalize") {
				t.Fatalf("publication=%t, %v", written, err)
			}
			want := "original complete bundle"
			switch phase {
			case "finalize":
				want = "new complete bundle"
			case "changed":
				want = "foreign writer changed output"
			}
			actual, err := os.ReadFile(path)
			if err != nil || string(actual) != want {
				t.Fatalf("lost existing output: %q, %v", actual, err)
			}
			files, err := os.ReadDir(filepath.Dir(path))
			if err != nil || len(files) != 1 {
				t.Fatalf("unowned temporary residue: %+v, %v", files, err)
			}
		})
	}
}

func TestBundlePathsProtectPlumbingAndLinks(t *testing.T) {
	base := t.TempDir()
	for _, name := range []string{".memdolt/server.pid", ".memdolt/config.toml", ".memdolt/embeddings.sqlite", ".memdolt/dolt/memory", ".memhub/project.sqlite", ".git/config", ".orchestrator/run.json"} {
		if err := BundlePath(filepath.Join(base, filepath.FromSlash(name))); err == nil {
			t.Fatalf("accepted protected path %s", name)
		}
	}
	if err := BundlePath("relative.json"); err == nil {
		t.Fatal("accepted unresolved relative path")
	}
	actual, link := t.TempDir(), filepath.Join(base, "link")
	if err := os.Symlink(actual, link); err != nil {
		t.Skipf("symlink creation is unavailable: %v", err)
	}
	if _, err := PublishBundle(context.Background(), filepath.Join(link, "bundle.json"), []byte("complete"), func(os.FileInfo) error { return nil }, func([]byte) error { return nil }); err == nil {
		t.Fatal("followed a linked output directory")
	}
}
