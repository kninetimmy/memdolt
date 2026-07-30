package layout_test

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/kninetimmy/memdolt/internal/layout"
)

func TestPathsAreRootedBeneathTheCallerSuppliedBase(t *testing.T) {
	base := t.TempDir()

	paths, err := layout.New(base)
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	cases := map[string]struct {
		got  string
		want string
	}{
		"memdolt dir":   {paths.Dir(), filepath.Join(base, ".memdolt")},
		"dolt data dir": {paths.DoltDataDir(), filepath.Join(base, ".memdolt", "dolt")},
		"lock file":     {paths.LockFile(), filepath.Join(base, ".memdolt", "LOCK")},
		"pidfile":       {paths.PidFile(), filepath.Join(base, ".memdolt", "server.pid")},
	}
	for name, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", name, tc.got, tc.want)
		}
	}
}

func TestNewRequiresABaseDirectory(t *testing.T) {
	if _, err := layout.New(""); !errors.Is(err, layout.ErrNoBaseDir) {
		t.Fatalf("error = %v, want one matching ErrNoBaseDir", err)
	}
}

func TestNewMakesRelativeBasesAbsolute(t *testing.T) {
	paths, err := layout.New(".")
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if !filepath.IsAbs(paths.Base()) {
		t.Fatalf("base = %q, want an absolute path", paths.Base())
	}
}
