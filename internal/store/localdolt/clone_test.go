package localdolt

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dolthub/dolt/go/libraries/doltcore/dbfactory"
	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	"github.com/dolthub/dolt/go/libraries/doltcore/env"
	"github.com/dolthub/dolt/go/libraries/utils/filesys"
	"github.com/dolthub/dolt/go/store/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/kninetimmy/memdolt/internal/singleowner"
	"github.com/kninetimmy/memdolt/internal/store"
)

func TestClonePreservesMainHistoryAndRows(t *testing.T) {
	ctx := context.Background()
	source := openInternalTestStore(t)
	if _, err := source.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	_, err := source.Commit(ctx, store.CommitRequest{
		Author: store.Actor{Name: "agent:clone-fixture", Email: "clone@example.invalid"}, Message: "synthetic clone provenance", NoText: true,
		Statements: []store.Statement{
			{SQL: "INSERT INTO tasks (id, title, status, created_at, updated_at) VALUES ('01K00000000000000000000000', 'clone task', 'open', NOW(), NOW())"},
			{SQL: "INSERT INTO session_notes (id, text, actor, actor_raw, created_at, session_id, agent_id, model_id, provider_id, variant) VALUES ('01K00000000000000000000001', 'clone note', 'agent:opencode', 'cli', NOW(), 'ses_synthetic', NULL, 'synthetic-model', NULL, '')"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	remote := pushCloneFixture(t, source, MainBranch)
	remoteURL, err := url.Parse(remote)
	if err != nil {
		t.Fatal(err)
	}
	remotePath := cloneFilePath(remoteURL)
	beforeSource := cloneFileTree(t, remotePath)
	wantHistory := cloneRows(t, source.db, "SELECT commit_hash, committer, email, date, message FROM dolt_log ORDER BY commit_hash")
	wantNotes := cloneRows(t, source.db, "SELECT * FROM session_notes AS OF 'main' ORDER BY id")
	wantTasks := cloneRows(t, source.db, "SELECT * FROM tasks AS OF 'main' ORDER BY id")
	var wantMain string
	if err := source.db.QueryRow("SELECT hash FROM dolt_branches WHERE name = 'main'").Scan(&wantMain); err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	base := cloneScratch(t)
	if err := os.Mkdir(filepath.Join(base, ".memdolt"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"config.toml", "embeddings.sqlite", "code_index.sqlite"} {
		if err := os.WriteFile(filepath.Join(base, ".memdolt", name), []byte("# preserve "+name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := source.cfg
	cfg.BaseDir = base
	result, err := Clone(ctx, cfg, remote, "")
	if err != nil {
		t.Fatal(err)
	}
	if result.MainCommit != wantMain || result.SchemaVersion != store.LatestSchemaVersion() {
		t.Fatalf("clone result = %+v, want main %s", result, wantMain)
	}
	if got := cloneFileTree(t, remotePath); !reflect.DeepEqual(got, beforeSource) {
		t.Fatal("successful clone changed its file source")
	}
	cloned, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := cloned.Open(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cloned.Close() }()
	for _, check := range []struct {
		query string
		want  string
	}{
		{"SELECT commit_hash, committer, email, date, message FROM dolt_log ORDER BY commit_hash", wantHistory},
		{"SELECT * FROM session_notes AS OF 'main' ORDER BY id", wantNotes},
		{"SELECT * FROM tasks AS OF 'main' ORDER BY id", wantTasks},
	} {
		if got := cloneRows(t, cloned.db, check.query); got != check.want {
			t.Fatalf("%s\ngot %s\nwant %s", check.query, got, check.want)
		}
	}
	var origin string
	if err := cloned.db.QueryRow("SELECT url FROM dolt_remotes WHERE name = 'origin'").Scan(&origin); err != nil || origin != remote {
		t.Fatalf("origin = %q, %v; want %s", origin, err, remote)
	}
	for _, name := range []string{"config.toml", "embeddings.sqlite", "code_index.sqlite"} {
		data, err := os.ReadFile(filepath.Join(base, ".memdolt", name))
		if err != nil || string(data) != "# preserve "+name {
			t.Fatalf("changed %s: %q, %v", name, data, err)
		}
	}
}

func TestCloneFileSourceRefusalsDoNotInitializeSource(t *testing.T) {
	for _, name := range []string{"empty", "unrelated file", "invalid manifest"} {
		t.Run(name, func(t *testing.T) {
			source := cloneScratch(t)
			if name != "empty" {
				file := "keep.txt"
				if name == "invalid manifest" {
					file = "manifest"
				}
				if err := os.WriteFile(filepath.Join(source, file), []byte("preserve this existing file"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before := cloneFileTree(t, source)
			path := filepath.ToSlash(source)
			if !strings.HasPrefix(path, "/") {
				path = "/" + path
			}
			remote := (&url.URL{Scheme: "file", Path: path}).String()
			cfg := Config{BaseDir: cloneScratch(t), Actor: store.Actor{Name: "user", Email: "user@example.invalid"}}
			if _, err := Clone(context.Background(), cfg, remote, ""); err == nil {
				t.Fatal("invalid file source was reported usable")
			}
			if got := cloneFileTree(t, source); !reflect.DeepEqual(got, before) {
				t.Fatal("refused clone changed its file source")
			}
		})
	}
}

func TestCloneFileSourceWithoutOldgenRemainsUnchanged(t *testing.T) {
	st := openInternalTestStore(t)
	if _, err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	remote := pushCloneFixture(t, st, MainBranch)
	u, err := url.Parse(remote)
	if err != nil {
		t.Fatal(err)
	}
	source := cloneFilePath(u)
	oldgen := filepath.Join(source, "oldgen")
	entries, err := os.ReadDir(oldgen)
	if err != nil || len(entries) != 0 {
		t.Fatalf("expected an empty fixture oldgen: %v, %v", entries, err)
	}
	if err := os.Remove(oldgen); err != nil {
		t.Fatal(err)
	}
	before := cloneFileTree(t, source)
	cfg := st.cfg
	cfg.BaseDir = cloneScratch(t)
	if result, err := Clone(context.Background(), cfg, remote, ""); err != nil || result.SchemaVersion != store.LatestSchemaVersion() {
		t.Fatalf("clone without source oldgen = %+v, %v", result, err)
	}
	if got := cloneFileTree(t, source); !reflect.DeepEqual(got, before) {
		t.Fatal("successful clone created oldgen or otherwise changed its source")
	}
}

// Reads can update access times at the OS level; names, bytes, modes and
// modification times are the source-preservation contract checked here.
type cloneFileSnapshot struct {
	mode  os.FileMode
	mtime time.Time
	hash  [32]byte
}

func cloneFileTree(t *testing.T, root string) map[string]cloneFileSnapshot {
	t.Helper()
	result := make(map[string]cloneFileSnapshot)
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		item := cloneFileSnapshot{mode: info.Mode(), mtime: info.ModTime()}
		if !entry.IsDir() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			item.hash = sha256.Sum256(data)
		}
		result[rel] = item
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return result
}

func pushCloneFixture(t *testing.T, st *Store, branch string) string {
	t.Helper()
	path := filepath.Join(cloneScratch(t), "remote with spaces")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	path = filepath.ToSlash(path)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	remote := (&url.URL{Scheme: "file", Path: path}).String()
	for _, statement := range []store.Statement{
		{SQL: "CALL DOLT_REMOTE('add', 'origin', ?)", Args: []any{remote}},
		{SQL: "CALL DOLT_PUSH('origin', ?)", Args: []any{branch}},
	} {
		if _, err := st.db.Exec(statement.SQL, statement.Args...); err != nil {
			t.Fatal(err)
		}
	}
	return remote
}

func cloneRows(t *testing.T, db *sql.DB, query string) string {
	t.Helper()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var result [][]any
	for rows.Next() {
		values := make([]any, len(cols))
		targets := make([]any, len(cols))
		for i := range values {
			targets[i] = &values[i]
		}
		if err := rows.Scan(targets...); err != nil {
			t.Fatal(err)
		}
		result = append(result, values)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestCloneRefusesInvalidAndUnsupportedStores(t *testing.T) {
	for _, tc := range []struct{ name, sql, want string }{
		{"missing metadata", "DROP TABLE meta", "not an initialized memdolt store"},
		{"newer", "UPDATE meta SET v = '999' WHERE k = 'schema_version'", "memdolt upgrade"},
		{"invalid version", "UPDATE meta SET v = 'invalid' WHERE k = 'schema_version'", "not a version number"},
		{"missing tasks", "DROP TABLE tasks", "invalid cloned memdolt schema"},
		{"missing facts", "DROP TABLE facts", "invalid cloned memdolt schema"},
		{"missing note provenance", "ALTER TABLE session_notes DROP COLUMN model_id", "invalid cloned memdolt schema"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := openInternalTestStore(t)
			if _, err := st.Migrate(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, err := st.Commit(context.Background(), store.CommitRequest{
				Author: st.cfg.Actor, Message: "unsupported clone fixture", NoText: true,
				Statements: []store.Statement{{SQL: tc.sql}},
			}); err != nil {
				t.Fatal(err)
			}
			remote := pushCloneFixture(t, st, MainBranch)
			cfg := st.cfg
			cfg.BaseDir = cloneScratch(t)
			_, err := Clone(context.Background(), cfg, remote, "")
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "artifacts retained at") {
				t.Fatalf("Clone = %v", err)
			}
			if _, err := os.Stat(filepath.Join(cfg.BaseDir, ".memdolt", "dolt", "memory", ".dolt", "noms", "manifest")); err != nil {
				t.Fatalf("unsupported clone was not retained: %v", err)
			}
		})
	}
}

// Match the existing embedded-store fixtures: Windows can retain mapped table
// files after close, so cleanup is best-effort rather than a test assertion.
func cloneScratch(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "memdolt-clone")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestCloneInputValidation(t *testing.T) {
	t.Setenv("DOLT_REMOTE_PASSWORD", "synthetic-password")
	for _, raw := range []string{"host/db", "ssh://host/db", "https:host/db", "https://host/db?password=secret", "https://host/db#secret", "http://user:secret@host/db", "file:relative", "file:/absolute", "file://server/share", "https://host", "http://host/\nsecret", "http://host/db%zz", "http://host:/db", "http://host:65536/db", "http://host:0/db", "http://host/db%0asecret"} {
		if err := validateCloneRemote(raw, ""); err == nil || strings.Contains(err.Error(), "secret") {
			t.Errorf("validate %q = %v", raw, err)
		}
	}
	for _, user := range []string{"user:secret", "user\n", " user", strings.Repeat("x", 33), "' OR 1=1"} {
		if err := validateCloneRemote("https://host/db", user); err == nil {
			t.Errorf("accepted invalid user %q", user)
		}
	}
	for _, raw := range []string{"https://host/db", "http://127.0.0.1:50051/project"} {
		if err := validateCloneRemote(raw, "test-user"); err != nil {
			t.Error(err)
		}
	}
	if err := os.Unsetenv("DOLT_REMOTE_PASSWORD"); err != nil {
		t.Fatal(err)
	}
	if err := validateCloneRemote("https://host/db", "test-user"); err == nil || !strings.Contains(err.Error(), "DOLT_REMOTE_PASSWORD") {
		t.Fatalf("missing password = %v", err)
	}
}

func TestCloneFSRefusesCleanupAndEscapes(t *testing.T) {
	root := t.TempDir()
	fs, err := newCloneFS(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "memory"), 0o700); err != nil {
		t.Fatal(err)
	}
	child, err := fs.WithWorkingDir("memory")
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "memory", "foreign.txt")
	if err := os.WriteFile(file, []byte("foreign content introduced during clone"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	for _, err := range []error{child.Delete(".", true), child.DeleteFile("foreign.txt"), child.MoveDir(".", "../moved"), child.MoveFile("foreign.txt", "moved"), child.WriteFile("../../escape", nil, 0o600), child.MkDirs("../../escape")} {
		if err == nil {
			t.Fatal("unsafe filesystem operation succeeded")
		}
	}
	after, err := os.ReadFile(file)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("foreign file changed: %q, %v", after, err)
	}
}

func TestCloneRefusesBeforeRemoteContact(t *testing.T) {
	st := openInternalTestStore(t)
	for _, ownerLive := range []bool{true, false} {
		if !ownerLive {
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
		}
		_, err := Clone(context.Background(), st.cfg, "http://127.0.0.1:1/never-contact", "")
		want := "not empty"
		if ownerLive {
			want = "exclusive ownership"
		}
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("Clone with live owner %t = %v", ownerLive, err)
		}
	}
	base := cloneScratch(t)
	cfg := st.cfg
	cfg.BaseDir = base
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Clone(ctx, cfg, "http://127.0.0.1:1/never-contact", ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled clone = %v", err)
	}
	for _, raw := range []string{"http://user:secret@127.0.0.1:1/db", "file:relative"} {
		if _, err := Clone(context.Background(), cfg, raw, ""); err == nil {
			t.Fatalf("invalid clone succeeded: %q", raw)
		}
	}
	for _, suffix := range []string{"percent%20path", "query?path"} {
		invalid := cfg
		invalid.BaseDir = filepath.Join(base, suffix)
		if _, err := Clone(context.Background(), invalid, "http://127.0.0.1:1/never-contact", ""); err == nil {
			t.Fatalf("unusable embedded destination accepted: %s", suffix)
		}
	}
	encodedTarget := filepath.Join(base, "resolved%20target")
	if err := os.Mkdir(encodedTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(encodedTarget, alias); err == nil {
		invalid := cfg
		invalid.BaseDir = alias
		if _, err := Clone(context.Background(), invalid, "http://127.0.0.1:1/never-contact", ""); err == nil || !strings.Contains(err.Error(), "contains '%'") {
			t.Fatalf("unusable resolved destination accepted: %v", err)
		}
	}
	if _, err := os.Stat(filepath.Join(base, ".memdolt")); !os.IsNotExist(err) {
		t.Fatalf("preflight refusal created state: %v", err)
	}
}

func TestCloneOlderSchemaRetainsHistoryForExplicitMigration(t *testing.T) {
	ctx := context.Background()
	st := openInternalTestStore(t)
	for _, migration := range store.Migrations()[:3] {
		if _, err := st.applyMigration(ctx, st.db, migration); err != nil {
			t.Fatal(err)
		}
	}
	remote := pushCloneFixture(t, st, MainBranch)
	wantHistory := cloneRows(t, st.db, "SELECT commit_hash FROM dolt_log ORDER BY commit_hash")
	cfg := st.cfg
	cfg.BaseDir = cloneScratch(t)
	if _, err := Clone(ctx, cfg, remote, ""); err == nil || !strings.Contains(err.Error(), "memdolt init") {
		t.Fatalf("old clone = %v", err)
	}
	cloned, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := cloned.Open(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cloned.Close() }()
	if got := cloneRows(t, cloned.db, "SELECT commit_hash FROM dolt_log ORDER BY commit_hash"); got != wantHistory {
		t.Fatalf("old clone changed history: %s, want %s", got, wantHistory)
	}
	result, err := cloned.Migrate(ctx)
	if err != nil || len(result.Applied) != 1 || result.Version != store.LatestSchemaVersion() {
		t.Fatalf("explicit migration after clone = %+v, %v", result, err)
	}
}

func TestCloneMissingMainAndCloseFailureRetainArtifacts(t *testing.T) {
	ctx := context.Background()
	st := openInternalTestStore(t)
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec("CALL DOLT_BRANCH('other')"); err != nil {
		t.Fatal(err)
	}
	remote := pushCloneFixture(t, st, "other")
	cfg := st.cfg
	cfg.BaseDir = cloneScratch(t)
	if _, err := Clone(ctx, cfg, remote, ""); err == nil || !strings.Contains(err.Error(), "main branch") || !strings.Contains(err.Error(), "artifacts retained at") {
		t.Fatalf("missing main clone = %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.BaseDir, ".memdolt", "dolt", "memory", ".dolt")); err != nil {
		t.Fatalf("failed transfer removed destination: %v", err)
	}
	if _, err := st.db.Exec("CALL DOLT_PUSH('origin', 'main')"); err != nil {
		t.Fatal(err)
	}
	cfg.BaseDir = cloneScratch(t)
	closeFailure := errors.New("synthetic close failure")
	result, err := clone(ctx, cfg, remote, "", func(lock *singleowner.Lock) error {
		return errors.Join(lock.Release(), closeFailure)
	})
	if result.MainCommit == "" || !errors.Is(err, closeFailure) || !strings.Contains(err.Error(), "artifacts retained at") {
		t.Fatalf("close failure = %+v, %v", result, err)
	}
}

func TestCloneAuthenticationAndCancellationAreLocalAndRedacted(t *testing.T) {
	const user, password = "synthetic-user", "synthetic-password"
	t.Setenv("DOLT_REMOTE_PASSWORD", password)
	for _, cancelTransfer := range []bool{false, true} {
		t.Run(map[bool]string{false: "authentication", true: "cancellation"}[cancelTransfer], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			base := cloneScratch(t)
			foreign := filepath.Join(base, ".memdolt", "dolt", "memory", "foreign.txt")
			auth := make(chan string, 1)
			server := grpc.NewServer(grpc.UnknownServiceHandler(func(_ any, stream grpc.ServerStream) error {
				md, _ := metadata.FromIncomingContext(stream.Context())
				select {
				case auth <- strings.Join(md.Get("authorization"), " "):
				default:
				}
				if err := os.WriteFile(foreign, []byte("introduced while contacting the remote"), 0o600); err != nil {
					return status.Error(codes.Internal, "fixture could not create foreign file")
				}
				if cancelTransfer {
					cancel()
					return status.Error(codes.Canceled, "fixture canceled")
				}
				return status.Error(codes.Unauthenticated, "synthetic authentication refusal "+password+" "+base64.StdEncoding.EncodeToString([]byte(user+":"+password)))
			}))
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- server.Serve(listener) }()
			defer func() { server.Stop(); <-done }()
			_, err = Clone(ctx, Config{BaseDir: base, Actor: store.Actor{Name: "user", Email: "user@example.invalid"}}, "http://"+listener.Addr().String()+"/fixture", user)
			if err == nil || strings.Contains(err.Error(), password) || strings.Contains(err.Error(), base64.StdEncoding.EncodeToString([]byte(user+":"+password))) || !strings.Contains(err.Error(), "artifacts retained at") {
				t.Fatalf("clone failure = %v", err)
			}
			select {
			case got := <-auth:
				if got != "Basic "+base64.StdEncoding.EncodeToString([]byte(user+":"+password)) {
					t.Fatal("driver did not send the requested synthetic identity and environment password")
				}
			default:
				t.Fatal("clone never reached the synthetic authentication path")
			}
			data, readErr := os.ReadFile(foreign)
			if readErr != nil || string(data) != "introduced while contacting the remote" {
				t.Fatalf("foreign content was not preserved: %q, %v", data, readErr)
			}
			if err := filepath.WalkDir(filepath.Join(base, ".memdolt"), func(path string, entry os.DirEntry, err error) error {
				if err != nil || entry.IsDir() {
					return err
				}
				data, err := os.ReadFile(path)
				if strings.Contains(string(data), password) {
					t.Error("password persisted in clone artifacts")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCloneInitializationFailureCannotDeleteForeignContent(t *testing.T) {
	root := t.TempDir()
	fs, err := newCloneFS(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.MkDirs(".clone-home/.dolt"); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(root, ".dolt", "foreign.txt")
	fault := &cloneInitFaultFS{Filesys: fs, foreign: foreign}
	dest := env.LoadWithoutDB(context.Background(), func() (string, error) { return filepath.Join(root, ".clone-home"), nil }, fault, doltdb.LocalDirDoltDB, "test")
	defer func() { _ = dest.Close() }()
	if err := dest.InitRepoWithNoData(context.Background(), types.Format_Default); err == nil {
		t.Fatal("initialization fault did not fail")
	}
	if data, err := os.ReadFile(foreign); err != nil || string(data) != "concurrent foreign content" {
		t.Fatalf("initialization cleanup removed foreign content: %q, %v", data, err)
	}
}

type cloneInitFaultFS struct {
	filesys.Filesys
	foreign string
}

func (f *cloneInitFaultFS) WriteFile(path string, data []byte, mode os.FileMode) error {
	if filepath.Base(path) == "config.json" {
		if err := os.WriteFile(f.foreign, []byte("concurrent foreign content"), 0o600); err != nil {
			return err
		}
		return errors.New("synthetic initialization write failure")
	}
	return f.Filesys.WriteFile(path, data, mode)
}

func TestCloneInspectionRetainsOldForeignTempFiles(t *testing.T) {
	ctx := context.Background()
	st := openInternalTestStore(t)
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	fs, err := newCloneFS(st.DataDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(st.DataDir(), ".clone-home")
	if err := fs.MkDirs(filepath.Join(home, ".dolt")); err != nil {
		t.Fatal(err)
	}
	dbfs, err := fs.WithWorkingDir(DatabaseName)
	if err != nil {
		t.Fatal(err)
	}
	dest := env.LoadWithoutDB(ctx, func() (string, error) { return home, nil }, dbfs, doltdb.LocalDirDoltDB, "test")
	dest.DBLoadParams = map[string]any{dbfactory.DisableSingletonCacheParam: struct{}{}}
	db := dest.DoltDB(ctx)
	defer func() { _ = dest.Close() }()
	if dest.DBLoadError != nil {
		t.Fatal(dest.DBLoadError)
	}
	temp, err := dest.TempTableFilesDir()
	if err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(temp, "old-foreign-file")
	if err := os.WriteFile(foreign, []byte("foreign content with preserved old mtime"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(foreign, old, old); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(foreign)
	if err != nil {
		t.Fatal(err)
	}
	result, err := inspectClone(ctx, st.DataDir(), db)
	if err != nil || result.MainCommit == "" || result.SchemaVersion != store.LatestSchemaVersion() {
		t.Fatalf("inspect committed clone = %+v, %v", result, err)
	}
	if err := dest.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(foreign)
	if err != nil || !after.ModTime().Equal(info.ModTime()) {
		t.Fatalf("inspection changed old foreign file: %v", err)
	}
	if data, err := os.ReadFile(foreign); err != nil || string(data) != "foreign content with preserved old mtime" {
		t.Fatalf("inspection changed old foreign contents: %q, %v", data, err)
	}
}
