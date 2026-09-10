package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kninetimmy/memdolt/internal/ipc"
	"github.com/kninetimmy/memdolt/internal/layout"
	"github.com/kninetimmy/memdolt/internal/memory"
	"github.com/kninetimmy/memdolt/internal/storeipc"
)

func bodyFileCommands() [][]string {
	return [][]string{{"note", "add"}, {"state", "set"}, {"arch", "set"}, {"opencode", "wrap-up-note", "ses_current"}}
}

func TestFileBodiesWriteAndReopenDirectAndOwner(t *testing.T) {
	fakeOpenCodeAPI(t, `{"data":{"id":"ses_current","agent":"build","model":{"providerID":" provider ","id":"model","variant":"max"}}}`, false)
	for _, routed := range []bool{false, true} {
		t.Run(fmt.Sprintf("owner=%t", routed), func(t *testing.T) {
			base := initStore(t)
			before := storeCommitCount(t, base)
			source := scratchDir(t)
			t.Chdir(source)
			stop := func() {}
			if routed {
				stop = serveTransferProcess(t, base)
			}
			var notes []noteInfo
			var narratives []narrativeInfo
			started := time.Now().UTC().Truncate(time.Second)
			for _, body := range []string{" \nCafé — 日本語 🐴\r\nsecond line\n\t", strings.Repeat("a", 65535), strings.Repeat("界", 21845)} {
				file := filepath.Join(source, "body café.txt")
				writeTestFile(t, file, body)
				// A repository-relative interpretation must not select this decoy.
				writeTestFile(t, filepath.Join(base, filepath.Base(file)), "wrong directory")
				for i, command := range bodyFileCommands() {
					path := file
					if i%2 == 0 {
						path = filepath.Base(file)
					}
					args := append(command, "--from-file", path, "--dir", base, "--json")
					if command[0] != "opencode" {
						args = append(args, "--actor", "Claude Code")
					}
					out := runMemdoltIn(t, "stdin must not win", args...)
					wantBody := strings.TrimSpace(body)
					if command[0] == "note" || command[0] == "opencode" {
						note := decodeJSON[noteInfo](t, out)
						if note.Text != wantBody || note.ID == "" || note.Commit == "" || note.CreatedAt == nil || note.CreatedAt.Before(started) || note.CreatedAt.After(time.Now()) {
							t.Fatalf("%s changed file bytes, identity, timestamp or commit (body bytes=%d)", command[0], len(body))
						}
						if command[0] == "opencode" {
							if note.Actor != "agent:opencode" || note.ActorRaw != "cli" || note.SessionID != "ses_current" || note.AgentID != "build" || note.ProviderID != " provider " || note.ModelID != "model" || note.Variant != "max" {
								t.Fatal("file note changed verified OpenCode provenance")
							}
						} else if note.Actor != "agent:claude-code" || note.ActorRaw != "Claude Code" || note.NoteProvenance != (memory.NoteProvenance{}) {
							t.Fatal("ordinary file note changed attribution or invented provenance")
						}
						notes = append(notes, note)
					} else {
						narrative := decodeJSON[narrativeInfo](t, out)
						if narrative.Body != wantBody || narrative.Kind != memory.NarrativeKind(command[0]) || narrative.Actor != "agent:claude-code" || narrative.ActorRaw != "Claude Code" || narrative.ID == "" || narrative.Commit == "" || narrative.CreatedAt == nil || narrative.CreatedAt.Before(started) || narrative.CreatedAt.After(time.Now()) {
							t.Fatalf("%s changed body, attribution, timestamp or commit (body bytes=%d)", command[0], len(body))
						}
						narratives = append(narratives, narrative)
					}
				}
			}
			stop()
			if storeCommitCount(t, base) != before+len(notes)+len(narratives) || activeBranch(t, base) != "main" {
				t.Fatal("file writes lost or repeated a commit, or changed branch")
			}
			log := commitLog(t, base)
			stored := decodeJSON[noteList](t, runMemdolt(t, "note", "list", "--dir", base, "--json")).Notes
			if len(stored) != len(notes) {
				t.Fatal("reopened notes changed row count")
			}
			for _, want := range notes {
				found := false
				for _, got := range stored {
					found = found || reflect.DeepEqual(got, want.Note)
				}
				actor, err := memory.NormalizeActor(want.Actor)
				if err != nil || !found || log[want.Commit].author != want.Actor || log[want.Commit].email != actor.CommitAuthor().Email || !strings.HasPrefix(log[want.Commit].message, "note add ") {
					t.Fatal("reopened note lost body, timestamp, provenance or attributed history")
				}
			}
			for _, kind := range []string{"state", "arch"} {
				history := decodeJSON[narrativeHistory](t, runMemdolt(t, kind, "history", "--dir", base, "--json")).History
				if len(history) != 3 {
					t.Fatal("file narrative overwrote history")
				}
				for _, want := range narratives {
					if string(want.Kind) != kind {
						continue
					}
					found := false
					for _, got := range history {
						found = found || reflect.DeepEqual(got, want.Narrative)
					}
					if !found || log[want.Commit] != (logEntry{"agent:claude-code", "agent-claude-code@memdolt.invalid", kind + " set"}) {
						t.Fatal("reopened narrative lost body, timestamp or attributed history")
					}
				}
			}
		})
	}
}

func TestFileBodyRefusalsPreserveMemoryAndMissingTargets(t *testing.T) {
	api := fakeOpenCodeAPI(t, `{"data":{"id":"ses_current"}}`, false)
	source := scratchDir(t)
	for name, body := range map[string]string{
		"valid": "safe text", "empty": "", "blank": " \n\t\u2003", "invalid": "body\xff",
		"large": strings.Repeat("a", 65536), "large-unicode": strings.Repeat("界", 21846),
		"large-before-trim": strings.Repeat(" ", 65535) + "x",
	} {
		writeTestFile(t, filepath.Join(source, name), body)
	}
	for _, target := range []string{"direct", "owner", "no metadata", "missing directory"} {
		t.Run(target, func(t *testing.T) {
			base := scratchDir(t)
			if target == "missing directory" {
				base = filepath.Join(base, "missing")
			}
			var before [][]string
			if target == "direct" || target == "owner" {
				runMemdolt(t, "init", "--dir", base)
				writeTestFile(t, pathsFor(t, base).ConfigFile(), "[deny_list]\npatterns=[]\n")
				if target == "owner" {
					serveStore(t, base)
				}
				humanInspect(t, base, func(st commandStore) { before = repoStateSnapshot(t, st) })
			}
			for _, tc := range []struct {
				args []string
				want string
			}{
				{[]string{"--from-file", filepath.Join(source, "missing")}, "resolve source"},
				{[]string{"--from-file", source}, "regular file"},
				{[]string{"--from-file", filepath.Join(source, "empty")}, "empty or whitespace-only"},
				{[]string{"--from-file", filepath.Join(source, "blank")}, "empty or whitespace-only"},
				{[]string{"--from-file", filepath.Join(source, "invalid")}, "UTF-8"},
				{[]string{"--from-file", filepath.Join(source, "large")}, "65535-byte"},
				{[]string{"--from-file", filepath.Join(source, "large-unicode")}, "65535-byte"},
				{[]string{"--from-file", filepath.Join(source, "large-before-trim")}, "65535-byte"},
				{[]string{"--from-file="}, "nonempty file path"},
				{[]string{"text", "--from-file", filepath.Join(source, "valid")}, "mutually exclusive"},
				{[]string{"", "--from-file", filepath.Join(source, "valid")}, "mutually exclusive"},
				{[]string{"text", "--from-file="}, "mutually exclusive"},
				{[]string{"", "--from-file="}, "mutually exclusive"},
			} {
				for _, command := range bodyFileCommands() {
					args := append(append(command, tc.args...), "--dir", base, "--json")
					out, err := runMemdoltResult(t, args...)
					// A missing repository refuses owner protection before content read.
					want := tc.want
					if target == "missing directory" && (want == "UTF-8" || want == "65535-byte" || want == "empty or whitespace-only") {
						want = "resolve repository for owner-file protection"
					}
					if err == nil || out != "" || !strings.Contains(err.Error(), want) {
						t.Fatalf("%v = %q, %v; want %s", command, out, err, want)
					}
				}
			}
			if before != nil {
				humanInspect(t, base, func(st commandStore) {
					if !reflect.DeepEqual(before, repoStateSnapshot(t, st)) {
						t.Fatal("source refusal changed native roots or branch heads")
					}
				})
				if raw, err := os.ReadFile(pathsFor(t, base).ConfigFile()); err != nil || string(raw) != "[deny_list]\npatterns=[]\n" {
					t.Fatal("source refusal changed configuration")
				}
			} else if _, err := os.Stat(pathsFor(t, base).Dir()); !os.IsNotExist(err) {
				t.Fatalf("source refusal created missing memory: %v", err)
			}
			if target == "missing directory" {
				if _, err := os.Stat(base); !os.IsNotExist(err) {
					t.Fatal("source refusal created missing target")
				}
			}
		})
	}
	if _, err := os.Stat(filepath.Join(api, "called")); !os.IsNotExist(err) {
		t.Fatal("source refusal invoked the OpenCode API")
	}
}

func TestFileBodyProtectsActualOwnerAndAliasesBeforeReading(t *testing.T) {
	api := fakeOpenCodeAPI(t, `{"data":{"id":"ses_current"}}`, false)
	base := initStore(t)
	before := storeCommitCount(t, base)
	stop := serveTransferProcess(t, base)
	owner := pathsFor(t, base).PidFile()
	raw, err := os.ReadFile(owner)
	if err != nil {
		t.Fatal(err)
	}
	record := decodeJSON[struct{ Detail struct{ Token string } }](t, string(raw))
	if record.Detail.Token == "" {
		t.Fatal("fixture did not publish an actual owner credential")
	}
	for _, alias := range []string{"owner", "hardlink", "symlink", "directory alias"} {
		t.Run(alias, func(t *testing.T) {
			file := owner
			switch alias {
			case "hardlink", "symlink":
				file = filepath.Join(scratchDir(t), "body.txt")
				link := os.Link
				if alias == "symlink" {
					link = os.Symlink
				}
				if err := link(owner, file); err != nil {
					t.Skipf("platform cannot create source alias: %v", err)
				}
			case "directory alias":
				dir := filepath.Join(scratchDir(t), "alias")
				if err := os.Symlink(filepath.Dir(owner), dir); err != nil {
					t.Skipf("platform cannot create directory alias: %v", err)
				}
				file = filepath.Join(dir, filepath.Base(owner))
			}
			for _, command := range bodyFileCommands() {
				for _, output := range [][]string{nil, {"--json"}} {
					args := append(append(command, "--from-file", file, "--dir", base), output...)
					out, err := runMemdoltResult(t, args...)
					if !errors.Is(err, layout.ErrOwnerSource) || out != "" || strings.Contains(err.Error(), record.Detail.Token) || strings.Contains(err.Error(), string(raw)) {
						t.Fatal("owner source did not refuse before reading without exposing credential content")
					}
				}
			}
		})
	}
	if after, err := os.ReadFile(owner); err != nil || string(after) != string(raw) {
		t.Fatal("source protection changed owner metadata")
	}
	if _, err := os.Stat(pathsFor(t, base).ConfigFile()); !os.IsNotExist(err) {
		t.Fatal("source protection created configuration")
	}
	if _, err := os.Stat(filepath.Join(api, "called")); !os.IsNotExist(err) {
		t.Fatal("owner source refusal invoked the OpenCode API")
	}
	stop()
	if storeCommitCount(t, base) != before || countRows(t, base, "SELECT COUNT(*) FROM session_notes") != 0 || countRows(t, base, "SELECT COUNT(*) FROM project_state") != 0 || countRows(t, base, "SELECT COUNT(*) FROM project_arch") != 0 {
		t.Fatal("protected owner content entered memory")
	}
}

func TestFileBodyOwnerProtectionFailsClosed(t *testing.T) {
	fakeOpenCodeAPI(t, `{"data":{"id":"ses_current"}}`, false)
	file := filepath.Join(scratchDir(t), "body.txt")
	writeTestFile(t, file, "ordinary selected text")
	for _, malformed := range []string{"metadata file", "owner directory", "metadata link", "owner link"} {
		t.Run(malformed, func(t *testing.T) {
			base := scratchDir(t)
			metadata := pathsFor(t, base).Dir()
			switch malformed {
			case "metadata file":
				writeTestFile(t, metadata, "not a directory")
			case "owner directory":
				if err := os.MkdirAll(pathsFor(t, base).PidFile(), 0o700); err != nil {
					t.Fatal(err)
				}
			case "metadata link":
				if err := os.Symlink(scratchDir(t), metadata); err != nil {
					t.Skipf("platform cannot create metadata alias: %v", err)
				}
			case "owner link":
				if err := os.Mkdir(metadata, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(file, pathsFor(t, base).PidFile()); err != nil {
					t.Skipf("platform cannot create owner alias: %v", err)
				}
			}
			for _, command := range bodyFileCommands() {
				out, err := runMemdoltResult(t, append(command, "--from-file", file, "--dir", base)...)
				if err == nil || out != "" || !strings.Contains(err.Error(), "protected memdolt owner metadata") {
					t.Fatalf("unverifiable owner protection = %q, %v", out, err)
				}
			}
			if _, err := os.Stat(pathsFor(t, base).DoltDataDir()); err == nil {
				t.Fatal("failed owner verification opened memory")
			}
		})
	}
}

func TestFileBodiesPreserveStdinArgumentsAndRenderedInputs(t *testing.T) {
	fakeOpenCodeAPI(t, `{"data":{"id":"ses_current"}}`, false)
	base := initStore(t)
	// Existing sources normalize before native TEXT storage; no new raw byte
	// cap or legacy 4096-character note cap may leak from the file-only route.
	body := strings.Repeat(" ", 65536) + strings.Repeat("é", 4097) + "\n"
	for _, command := range bodyFileCommands() {
		for _, fromStdin := range []bool{false, true} {
			args := append([]string{}, command...)
			if !fromStdin {
				args = append(args, body)
			}
			out := runMemdoltIn(t, body, append(args, "--dir", base, "--json")...)
			got := decodeJSON[struct{ Text, Body string }](t, out)
			if got.Text+got.Body != strings.TrimSpace(body) {
				t.Fatal("existing stdin/argument normalization changed")
			}
		}
	}
	runMemdolt(t, "render", "--dir", base)
	rendered := filepath.Join(base, ".memdolt", "rendered", "PROJECT.md")
	raw, err := os.ReadFile(rendered)
	if err != nil {
		t.Fatal(err)
	}
	// The sibling legacy render location remains a legitimate explicit source.
	legacy := filepath.Join(base, ".memhub", "rendered", "PROJECT.md")
	writeTestFile(t, legacy, string(raw))
	for _, file := range []string{rendered, legacy} {
		got := decodeJSON[narrativeInfo](t, runMemdolt(t, "state", "set", "--from-file", file, "--dir", base, "--json"))
		if got.Body != strings.TrimSpace(string(raw)) {
			t.Fatal("rendered narrative file changed")
		}
	}
	for _, command := range bodyFileCommands() {
		help := runMemdolt(t, append(command, "--help")...)
		for _, want := range []string{"--from-file", "65535", "standard input", "process", "owner credential"} {
			if !strings.Contains(help, want) {
				t.Fatalf("%v help omits %s", command, want)
			}
		}
	}
}

var errBodyFileClose = errors.New("synthetic body file close failure")

type bodyFileCloseFailure struct{ io.Reader }

func (bodyFileCloseFailure) Close() error { return errBodyFileClose }

func TestFileBodyReadCloseFailuresAndBoundedAllocation(t *testing.T) {
	body, err := readBodyFileContent(bodyFileCloseFailure{strings.NewReader("valid body")})
	if !errors.Is(err, errBodyFileClose) || body != "" {
		t.Fatal("source close failure returned a writable body")
	}
	file, err := os.CreateTemp(t.TempDir(), "closed")
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if body, err := readBodyFileContent(file); err == nil || body != "" {
		t.Fatal("native read/close failure returned a writable body")
	}
	large := strings.NewReader(strings.Repeat("a", 2*bodyFileLimit))
	if _, err := readBodyFileContent(io.NopCloser(large)); err == nil || large.Len() != bodyFileLimit-1 {
		t.Fatal("file reader consumed more than the limit plus one byte")
	}
}

func TestFileBodiesUnreadableSourcePreservesMissingMemory(t *testing.T) {
	fakeOpenCodeAPI(t, `{"data":{"id":"ses_current"}}`, false)
	file := filepath.Join(scratchDir(t), "unreadable.txt")
	writeTestFile(t, file, "source must never reach memory")
	makeBodyFileUnreadable(t, file)
	base := scratchDir(t)
	for _, command := range bodyFileCommands() {
		out, err := runMemdoltResult(t, append(command, "--from-file", file, "--dir", base, "--json")...)
		if err == nil || out != "" || !strings.Contains(err.Error(), "source") || strings.Contains(err.Error(), "source must never reach memory") {
			t.Fatalf("unreadable file = %q, %v", out, err)
		}
	}
	if _, err := os.Stat(pathsFor(t, base).Dir()); !os.IsNotExist(err) {
		t.Fatal("unreadable source opened memory")
	}
}

func TestFileBodiesRetainDenyGuards(t *testing.T) {
	fakeOpenCodeAPI(t, `{"data":{"id":"ses_current"}}`, false)
	for _, routed := range []bool{false, true} {
		t.Run(fmt.Sprintf("owner=%t", routed), func(t *testing.T) {
			base := initStore(t)
			writeDenyList(t, base, denyRule)
			if routed {
				serveStore(t, base)
			}
			var before [][]string
			humanInspect(t, base, func(st commandStore) { before = repoStateSnapshot(t, st) })
			file := filepath.Join(scratchDir(t), "body.txt")
			writeTestFile(t, file, theSecret)
			for _, command := range bodyFileCommands() {
				out, err := runMemdoltResult(t, append(command, "--from-file", file, "--dir", base, "--json")...)
				if err == nil || out != "" || !strings.Contains(err.Error(), "deny_list.patterns[0]") || strings.Contains(err.Error(), theSecret) {
					t.Fatal("file route changed deny refusal or leaked content")
				}
			}
			humanInspect(t, base, func(st commandStore) {
				if !reflect.DeepEqual(before, repoStateSnapshot(t, st)) {
					t.Fatal("denied file changed native roots or branch heads")
				}
			})
		})
	}
}

func TestFileBodiesLostOwnerReplyNeverReplays(t *testing.T) {
	fakeOpenCodeAPI(t, `{"data":{"id":"ses_current"}}`, false)
	base := initStore(t)
	before := storeCommitCount(t, base)
	st := openInitializedStore(t, base)
	t.Cleanup(func() { _ = st.Close() })
	backend := &localCommandStore{Store: st, baseDir: base}
	handler, err := storeipc.NewHandler(storeipc.Config{Store: backend, ReviewAccept: backend.ReviewAcceptExpected})
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	endpoint, err := ipc.Listen(ipc.Config{BaseDir: base, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == storeipc.CommitPath {
			calls.Add(1)
			handler.ServeHTTP(httptest.NewRecorder(), r)
			panic(http.ErrAbortHandler)
		}
		handler.ServeHTTP(w, r)
	})})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = endpoint.Close() })
	file := filepath.Join(scratchDir(t), "note.txt")
	writeTestFile(t, file, "one submitted file body")
	for i, command := range bodyFileCommands() {
		out, err := runMemdoltResult(t, append(command, "--from-file", file, "--dir", base, "--json")...)
		if err == nil || out != "" || !strings.Contains(err.Error(), "outcome unknown") || calls.Load() != int32(i+1) {
			t.Fatalf("file lost reply = %q, %v; submissions=%d", out, err, calls.Load())
		}
	}
	if err := endpoint.Close(); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if storeCommitCount(t, base) != before+4 || countRows(t, base, "SELECT COUNT(*) FROM session_notes") != 2 || countRows(t, base, "SELECT COUNT(*) FROM project_state") != 1 || countRows(t, base, "SELECT COUNT(*) FROM project_arch") != 1 {
		t.Fatal("unobserved file write was lost or replayed")
	}
	// Reopened bodies/provenance remain inspectable despite every lost reply.
	notes := decodeJSON[noteList](t, runMemdolt(t, "note", "list", "--dir", base, "--json")).Notes
	for _, note := range notes {
		if note.Text != "one submitted file body" || note.CreatedAt == nil || note.CreatedAt.IsZero() || (note.Actor == "agent:opencode" && note.SessionID != "ses_current") {
			t.Fatal("unknown file note lost its stored body, timestamp or provenance")
		}
	}
}
