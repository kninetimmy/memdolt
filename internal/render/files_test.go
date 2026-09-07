package render

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func fileFixture(t *testing.T) (config, [2]string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config{base: base, output: filepath.Join(base, ".memdolt", "rendered"), backup: filepath.Join(base, ".memdolt", "backups", "rendered")}
	old := [2]string{Marker + "\noriginal project\n", Marker + "\noriginal ledger\n"}
	if err := os.MkdirAll(cfg.output, 0o700); err != nil {
		t.Fatal(err)
	}
	for i, name := range filenames {
		if err := os.WriteFile(filepath.Join(cfg.output, name), []byte(old[i]), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return cfg, old
}

func TestFilesPreparationReplacementAndFinalizationFailures(t *testing.T) {
	boom := errors.New("injected inaccessible I/O failure")
	for _, phase := range []string{"prepare", "backup", "replace", "finalize"} {
		t.Run(phase, func(t *testing.T) {
			cfg, old := fileFixture(t)
			next := [2]string{Marker + "\nnext project\n", Marker + "\nnext ledger\n"}
			hooks := fileHooks{}
			failSecond := func(i int) error {
				if i == 1 {
					return boom
				}
				return nil
			}
			switch phase {
			case "prepare":
				hooks.beforePrepare = failSecond
			case "backup":
				hooks.beforeBackup = failSecond
			case "replace":
				hooks.beforeReplace = failSecond
			case "finalize":
				hooks.finalize = func() error { return boom }
			}
			result := Result{Status: "refused"}
			err := writeFiles(context.Background(), cfg, next, &result, hooks)
			if !errors.Is(err, boom) {
				t.Fatalf("%s failure = %v", phase, err)
			}
			written := map[string]int{"prepare": 0, "backup": 0, "replace": 1, "finalize": 2}[phase]
			if len(result.WrittenFiles) != written {
				t.Fatalf("lost confirmed file effects: %+v", result)
			}
			for i, name := range filenames {
				want := old[i]
				if i < written {
					want = next[i]
				}
				assertFile(t, filepath.Join(cfg.output, name), want)
			}
			for _, path := range result.BackupFiles {
				index := 0
				if strings.Contains(filepath.Base(path), "LEDGER") {
					index = 1
				}
				assertFile(t, path, old[index])
			}
			assertOnlyOutputs(t, cfg.output)
		})
	}
}

func TestFilesRefuseForeignDestinationsAndPreserveConcurrentEdits(t *testing.T) {
	for _, kind := range []string{"directory", "unmarked", "changed-after-backup", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			cfg, old := fileFixture(t)
			ledger := filepath.Join(cfg.output, filenames[1])
			hooks := fileHooks{}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch kind {
			case "directory":
				if err := os.Remove(ledger); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(ledger, 0o700); err != nil {
					t.Fatal(err)
				}
			case "unmarked":
				if err := os.WriteFile(ledger, []byte("user-owned content"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "changed-after-backup":
				hooks.beforeReplace = func(i int) error {
					if i == 0 {
						return os.WriteFile(filepath.Join(cfg.output, filenames[0]), []byte("foreign edit"), 0o600)
					}
					return nil
				}
			case "cancel":
				hooks.beforePrepare = func(i int) error {
					if i == 1 {
						cancel()
					}
					return nil
				}
			}
			result := Result{}
			if err := writeFiles(ctx, cfg, [2]string{"new project", "new ledger"}, &result, hooks); err == nil || len(result.WrittenFiles) != 0 {
				t.Fatalf("unsafe replacement=%+v, %v", result, err)
			}
			if kind == "changed-after-backup" {
				assertFile(t, filepath.Join(cfg.output, filenames[0]), "foreign edit")
			} else {
				assertFile(t, filepath.Join(cfg.output, filenames[0]), old[0])
			}
			if kind == "unmarked" {
				assertFile(t, ledger, "user-owned content")
			}
		})
	}
}

func TestConcurrentOutputGenerationsRefuseAndBackupsPrecedeReplacement(t *testing.T) {
	cfg, old := fileFixture(t)
	entered, resume, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	first := Result{}
	go func() {
		done <- writeFiles(context.Background(), cfg, [2]string{Marker + "\nfirst generation", Marker + "\nfirst ledger"}, &first, fileHooks{
			beforeReplace: func(i int) error {
				if i == 0 {
					close(entered)
					<-resume
				}
				return nil
			},
		})
	}()
	<-entered
	if len(first.BackupFiles) != 2 {
		t.Error("both backups must exist before the first replacement")
	}
	for i, path := range first.BackupFiles {
		assertFile(t, path, old[i])
	}
	second := Result{}
	err := writeFiles(context.Background(), cfg, [2]string{"second", "second"}, &second, fileHooks{})
	if err == nil || !strings.Contains(err.Error(), "locked") || len(second.WrittenFiles) != 0 {
		t.Errorf("concurrent render=%+v, %v", second, err)
	}
	close(resume)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	assertFile(t, filepath.Join(cfg.output, filenames[0]), Marker+"\nfirst generation")
	assertOnlyOutputs(t, cfg.output)
}

func TestOutputPathsRefuseTraversalMetadataAndSymlinks(t *testing.T) {
	cfg, _ := fileFixture(t)
	for _, output := range []string{"../escape", ".memdolt", ".memdolt/dolt/rendered", ".memdolt/config.toml", ".memdolt/embeddings.sqlite", ".memdolt/code_index.sqlite", ".memdolt/backups/rendered", ".git/objects", ".memhub/rendered", ".orchestrator/artifacts"} {
		configPath := filepath.Join(cfg.base, ".memdolt", "config.toml")
		if err := os.WriteFile(configPath, []byte("[render]\noutput_dir = '"+output+"'\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadConfig(cfg.base); err == nil {
			t.Errorf("accepted unsafe output %s", output)
		}
	}
	other, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(cfg.base, "output-link")
	if err := os.Symlink(other, link); err != nil {
		t.Skipf("platform does not permit symlink creation: %v", err)
	}
	if root, err := openDir(link, true); err == nil {
		_ = root.Close()
		t.Fatal("accepted symlink output root")
	}
	if runtime.GOOS == "windows" {
		if root, err := openDir(filepath.Join(cfg.base, "views."), true); err == nil {
			_ = root.Close()
			t.Fatal("accepted Windows path alias")
		}
	}
	entries, err := os.ReadDir(other)
	if err != nil || len(entries) != 0 {
		t.Fatalf("unsafe target changed: %v, %v", entries, err)
	}
}

func TestWindowsReplacementRefusalKeepsOriginalAndReportsPrefix(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows read-only destination exercises its native rename refusal")
	}
	cfg, old := fileFixture(t)
	ledger := filepath.Join(cfg.output, filenames[1])
	if err := os.Chmod(ledger, 0o400); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ledger, 0o600) })
	result := Result{}
	err := writeFiles(context.Background(), cfg, [2]string{Marker + "\nreplaced first", Marker + "\nreplace refused"}, &result, fileHooks{})
	if err == nil || !strings.Contains(err.Error(), "replace PROJECT_LEDGER.md") || result.Status != "partial" || len(result.WrittenFiles) != 1 || len(result.BackupFiles) != 2 {
		t.Fatalf("native replacement refusal=%+v, %v", result, err)
	}
	assertFile(t, ledger, old[1])
	assertFile(t, result.BackupFiles[0], old[0])
	assertFile(t, result.BackupFiles[1], old[1])
	assertOnlyOutputs(t, cfg.output)
}

func TestConfiguredExternalOutputAndUnrelatedFiles(t *testing.T) {
	cfg, _ := fileFixture(t)
	external, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(cfg.base, ".memdolt", "config.toml")
	configuration := "project_name = 'Configured project'\n[render]\noutput_dir = '" + external + "'\n[retrieval]\nfact_stale_after_days = 9223372036854775807\n"
	if err := os.WriteFile(configPath, []byte(configuration), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadConfig(cfg.base)
	if err != nil || loaded.output != external || loaded.backup != cfg.backup || loaded.project != "Configured project" {
		t.Fatalf("configured routing=%+v, %v", loaded, err)
	}
	keep := filepath.Join(external, "unrelated.txt")
	if err := os.WriteFile(keep, []byte("preserve me"), 0o600); err != nil {
		t.Fatal(err)
	}
	result := Result{}
	if err := writeFiles(context.Background(), loaded, [2]string{Marker + "\nproject", Marker + "\nledger"}, &result, fileHooks{}); err != nil {
		t.Fatal(err)
	}
	assertFile(t, keep, "preserve me")
	assertFile(t, configPath, configuration)
	for _, text := range []string{"[render]\noutput_dir = 4", "[render]\noutput_dir = ''", "[render]\noutput_dri = 'x'", "[retrieval]\nfact_stale_after_days = 0"} {
		if err := os.WriteFile(configPath, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadConfig(cfg.base); err == nil {
			t.Fatalf("accepted invalid consumed configuration %q", text)
		}
	}
}

func assertFile(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || string(got) != want {
		t.Fatalf("%s=%q, %v; want %q", path, got, err, want)
	}
}

func assertOnlyOutputs(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("temporary artifacts remain: %v, %v", entries, err)
	}
}
