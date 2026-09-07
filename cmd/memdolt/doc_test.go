package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/kninetimmy/memdolt/internal/ipc"
	"github.com/kninetimmy/memdolt/internal/memory"
	"github.com/kninetimmy/memdolt/internal/render"
	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

func TestDocumentCLIDirectAndOwnerLifecycle(t *testing.T) {
	for _, routed := range []bool{false, true} {
		t.Run(fmt.Sprintf("owner=%t", routed), func(t *testing.T) {
			base := initStore(t)
			if routed {
				serveStore(t, base)
			}
			if out := runMemdolt(t, "doc", "ls", "--dir", base, "--json"); out != "{\"documents\":[]}\n" {
				t.Fatalf("empty documents = %s", out)
			}
			// CLI paths are explicitly selected outside the repo and are resolved
			// relative to the caller before submission to an existing live owner.
			sourceDir := t.TempDir()
			t.Chdir(sourceDir)
			file := "spec's reference.md"
			writeTestFile(t, file, "# Title\n\n## 子\n\nUnicode 内容\n")
			added := decodeJSON[localdolt.DocResult](t, runMemdolt(t, "doc", "add", file, "--title", "Selected title", "--actor", "Claude Code", "--dir", base, "--json"))
			if added.Status != "created" || added.Document.Title != "Selected title" || added.Document.Path != filepath.Join(sourceDir, file) || added.Document.ChunkCount != 2 || !added.EnabledDefaultRecall {
				t.Fatalf("CLI add = %+v", added)
			}
			shown := decodeJSON[localdolt.DocResult](t, runMemdolt(t, "doc", "show", file, "--dir", base, "--json"))
			if !reflect.DeepEqual(added.Document, shown.Document) || len(shown.Chunks) != 2 || shown.Chunks[1].HeadingPath != "Title > 子" {
				t.Fatalf("CLI show = %+v", shown)
			}
			if out := runMemdolt(t, "doc", "show", added.Document.ID, "--dir", base); !strings.Contains(out, "Title > 子") || !strings.Contains(out, "Unicode 内容") {
				t.Fatalf("human show = %s", out)
			}
			again := decodeJSON[localdolt.DocResult](t, runMemdolt(t, "doc", "add", file, "--title", "Ignored", "--dir", base, "--json"))
			if again.Status != "unchanged" || again.Document.ID != added.Document.ID || again.Commit != "" || again.Document.Title != "Selected title" {
				t.Fatalf("CLI no-op = %+v", again)
			}
			writeTestFile(t, file, "# Changed\n\nA new paragraph\n")
			updated := decodeJSON[localdolt.DocResult](t, runMemdolt(t, "doc", "add", file, "--dir", base, "--json"))
			if updated.Status != "updated" || updated.Document.ID != added.Document.ID || updated.Document.ChunkCount != 1 || updated.EnabledDefaultRecall {
				t.Fatalf("CLI update = %+v", updated)
			}
			listed := decodeJSON[docListReport](t, runMemdolt(t, "doc", "list", "--dir", base, "--json"))
			if len(listed.Documents) != 1 || listed.Documents[0] != *updated.Document {
				t.Fatalf("CLI list = %+v", listed)
			}
			if err := os.Remove(file); err != nil {
				t.Fatal(err)
			}
			removed := decodeJSON[localdolt.DocResult](t, runMemdolt(t, "doc", "rm", file, "--dir", base, "--json"))
			if removed.Status != "removed" || removed.Document.ID != added.Document.ID || removed.Commit == "" {
				t.Fatalf("CLI removal = %+v", removed)
			}
			for _, operation := range []string{"show", "rm"} {
				missing := decodeJSON[localdolt.DocResult](t, runMemdolt(t, "doc", operation, added.Document.ID, "--dir", base, "--json"))
				if missing.Status != "not-found" || missing.Document != nil || missing.Commit != "" {
					t.Fatalf("not-found = %+v", missing)
				}
			}
			if got := runMemdolt(t, "doc", "ls", "--dir", base); got != "no documents\n" {
				t.Fatalf("list after remove = %s", got)
			}
		})
	}
}

func TestDocumentRenderIntegrationDirectAndOwner(t *testing.T) {
	for _, routed := range []bool{false, true} {
		t.Run(fmt.Sprintf("owner=%t", routed), func(t *testing.T) {
			base := initStore(t)
			writeTestFile(t, pathsFor(t, base).ConfigFile(), "project_name='Document render integration'\n[render]\noutput_dir='memory-view'\n[retrieval]\ninclude_docs_in_default=false\n")
			if routed {
				serveStore(t, base)
			}
			file := filepath.Join(base, "reference.md")
			const referenceBody = "unique reference body stays in document chunks"
			writeTestFile(t, file, "# Reference\n\n"+referenceBody+"\n")
			added := decodeJSON[localdolt.DocResult](t, runMemdolt(t, "doc", "add", file, "--actor", "integration-agent", "--dir", base, "--json"))
			if !added.EnabledDefaultRecall || added.Document.Source != "user" {
				t.Fatalf("first document = %+v", added)
			}
			shown := decodeJSON[localdolt.DocResult](t, runMemdolt(t, "doc", "show", added.Document.ID, "--dir", base, "--json"))
			for generation := range 2 {
				rendered := decodeJSON[render.Result](t, runMemdolt(t, "render", "--dir", base, "--json"))
				if rendered.Status != "written" || rendered.SourceCommit != added.Commit || rendered.OutputDir != filepath.Join(base, "memory-view") || len(rendered.WrittenFiles) != 2 || len(rendered.BackupFiles) != generation*2 {
					t.Fatalf("render generation %d = %+v", generation, rendered)
				}
				project, err := os.ReadFile(filepath.Join(rendered.OutputDir, "PROJECT.md"))
				if err != nil {
					t.Fatal(err)
				}
				ledger, err := os.ReadFile(filepath.Join(rendered.OutputDir, "PROJECT_LEDGER.md"))
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Contains(project, []byte("# Document render integration")) || !bytes.Contains(ledger, []byte("doc add "+added.Document.ID)) || !bytes.Contains(ledger, []byte("agent:integration-agent")) || bytes.Contains(ledger, []byte(referenceBody)) {
					t.Fatal("render lost configured naming or document commit provenance, or changed its category set")
				}
				for _, local := range []bool{true, false} {
					args := []string{"repo", "status", "--dir", base, "--json"}
					want := "no-remote"
					if local {
						args = append(args, "--local")
						want = "offline"
					}
					status := decodeJSON[localdolt.RepoStatusReport](t, runMemdolt(t, args...))
					if status.Status != want || status.MainCommit != added.Commit || !status.Clean {
						t.Fatalf("document repository status local=%t: %+v", local, status)
					}
				}
				after := decodeJSON[localdolt.DocResult](t, runMemdolt(t, "doc", "show", added.Document.ID, "--dir", base, "--json"))
				if !reflect.DeepEqual(shown, after) {
					t.Fatalf("render changed document metadata/chunks: before=%+v after=%+v", shown, after)
				}
				again := decodeJSON[localdolt.DocResult](t, runMemdolt(t, "doc", "add", file, "--dir", base, "--json"))
				if again.Status != "unchanged" || again.Commit != "" || !reflect.DeepEqual(again.Document, added.Document) {
					t.Fatalf("render changed hash no-op behavior: %+v", again)
				}
			}
		})
	}
}

func TestDocumentCLIMissingUnsupportedAndOwnerFailures(t *testing.T) {
	missing := t.TempDir()
	for _, args := range [][]string{{"add", "missing.md"}, {"ls"}, {"show", "missing"}, {"rm", "missing"}} {
		out, err := runMemdoltResult(t, append(append([]string{"doc"}, args...), "--dir", missing, "--json")...)
		if err == nil || !strings.Contains(err.Error(), "memdolt init") || out != "" {
			t.Fatalf("missing store = %q, %v", out, err)
		}
		if _, err := os.Stat(filepath.Join(missing, ".memdolt")); !os.IsNotExist(err) {
			t.Fatalf("missing store was initialized: %v", err)
		}
	}
	for _, version := range []string{"3", "99"} {
		base := initStore(t)
		st := openInitializedStore(t, base)
		if _, err := st.Commit(context.Background(), store.CommitRequest{Author: cliActor, Message: "fixture version", NoText: true, Statements: []store.Statement{
			{SQL: "UPDATE meta SET v = ? WHERE k = 'schema_version'", Args: []any{version}},
		}}); err != nil {
			t.Fatal(err)
		}
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}
		if out, err := runMemdoltResult(t, "doc", "ls", "--dir", base, "--json"); err == nil || out != "" {
			t.Fatalf("unsupported version %s = %q, %v", version, out, err)
		}
	}
	base := initStore(t)
	// A fallback would recreate this released fixture lock.
	if err := os.Remove(pathsFor(t, base).LockFile()); err != nil {
		t.Fatal(err)
	}
	endpoint, err := ipc.Listen(ipc.Config{BaseDir: base, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "synthetic owner authentication refusal", http.StatusUnauthorized)
	})})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = endpoint.Close() }()
	for _, args := range [][]string{{"add", "missing.md"}, {"ls"}, {"show", "missing"}, {"rm", "missing"}} {
		out, err := runMemdoltResult(t, append(append([]string{"doc"}, args...), "--dir", base, "--json")...)
		if err == nil || !strings.Contains(err.Error(), "401") || out != "" {
			t.Fatalf("owner refusal = %q, %v", out, err)
		}
		if _, err := os.Stat(pathsFor(t, base).LockFile()); !os.IsNotExist(err) {
			t.Fatalf("owner failure attempted direct fallback: %v", err)
		}
	}
	if help := runMemdolt(t, "doc", "--help"); strings.Contains(help, "--global") || !strings.Contains(help, "2000-character") || !strings.Contains(help, "allowed_dirs") {
		t.Fatalf("document help = %s", help)
	}
	if err := runMemdoltErr(t, "doc", "add", "file.md", "--global"); !strings.Contains(err, "unknown flag") {
		t.Fatal(err)
	}
}

func TestDocumentCLIReportsOutputAndCloseAfterCommit(t *testing.T) {
	for _, failure := range []string{"close", "human", "json"} {
		t.Run(failure, func(t *testing.T) {
			base := initStore(t)
			file := filepath.Join(base, "doc.md")
			writeTestFile(t, file, "# Confirmed\n")
			fixtureError := errors.New("synthetic document finalization failure")
			st := &repoFaultStore{commandStore: &localCommandStore{Store: openInitializedStore(t, base), baseDir: base}}
			cmd := &cobra.Command{Use: "doc"}
			cmd.SetContext(context.Background())
			out := &bytes.Buffer{}
			cmd.SetOut(out)
			jsonOutput = failure != "human"
			if failure == "close" {
				st.closeErr = fixtureError
			} else {
				cmd.SetOut(repoFailWriter{fixtureError})
			}
			err := runDoc(cmd, st, "add", file, "", memory.UserActor)
			if !errors.Is(err, fixtureError) || !st.closed || !strings.Contains(err.Error(), "confirmed created") {
				t.Fatalf("finalization = %v, closed=%t", err, st.closed)
			}
			if failure == "close" {
				result := decodeJSON[localdolt.DocResult](t, out.String())
				if result.Status != "created" || result.Error == "" || result.Commit == "" {
					t.Fatalf("lost confirmed output = %+v", result)
				}
			}
			check := openInitializedStore(t, base)
			result, err := check.DocShow(context.Background(), file)
			if err != nil || result.Status != "found" || result.Document.Title != "Confirmed" {
				t.Fatalf("post-error durable document = %+v, %v", result, err)
			}
		})
	}
}

func TestDocumentCLIProtectsOwnerMetadataAndPreservesRootFreedom(t *testing.T) {
	for _, configuration := range []string{"absent", "empty"} {
		t.Run(configuration, func(t *testing.T) {
			base := initStore(t)
			paths := pathsFor(t, base)
			const synthetic = "syntheticownercliregression"
			writeTestFile(t, paths.PidFile(), `{"pid":1,"host":"synthetic","id":"probe","detail":{"port":1,"token":"`+synthetic+`"}}`)
			if configuration == "empty" {
				writeTestFile(t, paths.ConfigFile(), "[deny_list]\npatterns=[]\n")
			}
			before := storeCommitCount(t, base)
			refused := func(t *testing.T, file string) {
				t.Helper()
				for _, asJSON := range []bool{false, true} {
					args := []string{"doc", "add", file, "--dir", base}
					if asJSON {
						args = append(args, "--json")
					}
					out, err := runMemdoltResult(t, args...)
					if err == nil || !strings.Contains(err.Error(), "protected memdolt owner metadata") || strings.Contains(err.Error(), synthetic) || out != "" {
						t.Fatal("CLI did not refuse owner metadata without returning content")
					}
				}
				if storeCommitCount(t, base) != before || countRows(t, base, "SELECT COUNT(*) FROM documents") != 0 || countRows(t, base, "SELECT COUNT(*) FROM doc_chunks") != 0 || countRows(t, base, "SELECT COUNT(*) FROM dolt_status") != 0 {
					t.Fatal("CLI owner-metadata refusal changed memory")
				}
				config, err := os.ReadFile(paths.ConfigFile())
				if configuration == "absent" {
					if !os.IsNotExist(err) {
						t.Fatal("CLI owner-metadata refusal created configuration")
					}
				} else if err != nil || string(config) != "[deny_list]\npatterns=[]\n" {
					t.Fatal("CLI owner-metadata refusal changed configuration")
				}
			}
			refused(t, paths.PidFile())
			for _, alias := range []string{"symlink", "hardlink"} {
				t.Run(alias, func(t *testing.T) {
					file := filepath.Join(base, alias+".md")
					link := os.Symlink
					if alias == "hardlink" {
						link = os.Link
					}
					if err := link(paths.PidFile(), file); err != nil {
						t.Skipf("platform cannot create fixture alias: %v", err)
					}
					refused(t, file)
				})
			}
			outside := filepath.Join(t.TempDir(), "ordinary.md")
			writeTestFile(t, outside, "# Ordinary external reference\n")
			allowed := decodeJSON[localdolt.DocResult](t, runMemdolt(t, "doc", "add", outside, "--dir", base, "--json"))
			if allowed.Status != "created" || allowed.Document.Path != outside {
				t.Fatal("owner credential protection changed ordinary CLI root freedom")
			}
		})
	}
}
