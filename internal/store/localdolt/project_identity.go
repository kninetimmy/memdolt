package localdolt

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/kninetimmy/memdolt/internal/store"
)

// ProjectIdentity stores only a credential-free canonical origin, never a host
// path. The full origin distinguishes collisions in memhub's short project ID.
type ProjectIdentity struct {
	ProjectID string `json:"projectId,omitempty"`
	Origin    string `json:"projectOrigin,omitempty"`
	Database  string `json:"hubDatabase,omitempty"`
}

var gitOriginPattern = regexp.MustCompile(`^(?:https://|ssh://(?:[A-Za-z0-9_.-]+@)?|(?:[A-Za-z0-9_.-]+@)?)([A-Za-z0-9][A-Za-z0-9.-]*)([:/])([A-Za-z0-9_.-]+(?:/[A-Za-z0-9_.-]+)+)/*$`)

func projectIdentity(raw string) (ProjectIdentity, error) {
	raw = strings.ToLower(raw)
	match := gitOriginPattern.FindStringSubmatch(raw)
	if match == nil || (strings.Contains(raw, "://") && match[2] != "/") ||
		(!strings.Contains(raw, "://") && match[2] != ":") {
		return ProjectIdentity{}, errors.New("unsupported Git origin; use credential-free HTTPS, ssh:// or SCP-style SSH with host/owner/repository and no port, query, fragment or escapes")
	}
	for _, label := range strings.Split(match[1], ".") {
		if label == "" || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return ProjectIdentity{}, errors.New("git origin requires an unambiguous host name")
		}
	}
	path := strings.TrimSuffix(strings.ToLower(match[3]), ".git")
	for _, part := range strings.Split(path, "/") {
		if part == "" || part == "." || part == ".." {
			return ProjectIdentity{}, errors.New("git origin contains an ambiguous repository path")
		}
	}
	origin := strings.ToLower(match[1]) + "/" + path
	tail := path[strings.LastIndex(path, "/")+1:]
	slug := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, tail)
	slug = strings.Trim(slug, "-")
	if slug == "" {
		slug = "repo"
	}
	if len(slug) > 32 {
		slug = slug[:32]
	}
	hash := sha256.Sum256([]byte(origin))
	id := fmt.Sprintf("%s-%x", slug, hash[:4])
	return ProjectIdentity{ProjectID: id, Origin: origin, Database: "proj_" + strings.ReplaceAll(id, "-", "_")}, nil
}

// ResolveProjectIdentity is offline. Absence of .git or remote.origin.url is
// local-only; invalid Git configuration, multiple origins and process failures
// are refusals. Never include Git output (which can contain secrets) in errors.
func ResolveProjectIdentity(ctx context.Context, base string) (ProjectIdentity, error) {
	if _, err := os.Lstat(filepath.Join(base, ".git")); os.IsNotExist(err) {
		return ProjectIdentity{}, nil
	} else if err != nil {
		return ProjectIdentity{}, fmt.Errorf("inspect Git repository identity: %w", err)
	}
	cmd := exec.CommandContext(ctx, "git", "-C", base, "config", "--local", "--null", "--get-all", "remote.origin.url")
	// An inherited GIT_DIR or injected config must not select another repository.
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if !strings.HasPrefix(strings.ToUpper(key), "GIT_") {
			cmd.Env = append(cmd.Env, item)
		}
	}
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return ProjectIdentity{}, fmt.Errorf("resolve Git identity canceled: %w", ctx.Err())
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 && len(out) == 0 && len(exit.Stderr) == 0 {
			return ProjectIdentity{}, nil
		}
		return ProjectIdentity{}, errors.New("cannot read local Git origin; inspect Git installation and repository configuration (no remote was contacted)")
	}
	if len(out) == 0 || out[len(out)-1] != 0 || strings.Count(string(out), "\x00") != 1 {
		return ProjectIdentity{}, errors.New("git origin must contain exactly one nonempty URL; inspect local Git configuration")
	}
	return projectIdentity(string(out[:len(out)-1]))
}

func identityFromMetadata(values map[string]string) (ProjectIdentity, error) {
	id, hasID := values["project_id"]
	origin, hasOrigin := values["project_origin"]
	if !hasID && !hasOrigin {
		return ProjectIdentity{}, nil
	}
	parsed, err := projectIdentity("https://" + origin)
	if err != nil || !hasID || !hasOrigin || parsed.ProjectID != id || parsed.Origin != origin {
		return ProjectIdentity{}, errors.New("invalid committed project identity; inspect meta.project_id and meta.project_origin with the owner stopped; never guess or overwrite identity")
	}
	return parsed, nil
}

func readProjectIdentity(ctx context.Context, q rowQuerier, revision string) (ProjectIdentity, error) {
	if revision != MainBranch && !transferHash.MatchString(revision) {
		return ProjectIdentity{}, errors.New("invalid identity revision")
	}
	values := map[string]string{}
	for _, key := range []string{"project_id", "project_origin"} {
		var actual, value string
		err := q.QueryRowContext(ctx, "SELECT k, v FROM "+quoteIdentifier(DatabaseName+"/"+revision)+".meta WHERE k = ?", key).Scan(&actual, &value)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil || actual != key {
			return ProjectIdentity{}, errors.New("cannot read committed project identity; inspect meta with the owner stopped")
		}
		values[key] = value
	}
	return identityFromMetadata(values)
}

func matchProjectIdentity(expected, incoming ProjectIdentity) error {
	if expected != incoming {
		return errors.New("project identity mismatch or short-ID collision; inspect Git origin and committed meta on both sides; restore the intended origin or select the correct store, never automatically reassign identity")
	}
	return nil
}

func (s *Store) resolvedIdentity(ctx context.Context) (ProjectIdentity, error) {
	if s.cfg.Global {
		return ProjectIdentity{}, nil
	}
	return ResolveProjectIdentity(ctx, s.paths.Base())
}

func (s *Store) checkProjectIdentity(ctx context.Context, q rowQuerier, revision string, require bool) (ProjectIdentity, error) {
	identity, err := readProjectIdentity(ctx, q, revision)
	if err != nil {
		return identity, err
	}
	expected, err := s.resolvedIdentity(ctx)
	if err != nil {
		return identity, err
	}
	if s.cfg.Global {
		return identity, matchProjectIdentity(ProjectIdentity{}, identity)
	}
	if expected.ProjectID != "" {
		if identity.ProjectID == "" && !require {
			return identity, nil
		}
		if identity.ProjectID == "" {
			return identity, errors.New("store has no project identity; stop the owner, review Git origin, then run `memdolt init --adopt-identity --dir <repository>` before transfer")
		}
		return identity, matchProjectIdentity(expected, identity)
	}
	return identity, nil
}

type IdentityResult struct {
	ProjectIdentity
	Commit string `json:"identityCommit,omitempty"`
}

// InitializeIdentity is an explicit terminal initialization step, not Open or
// Migrate. Adoption adds one user-attributed commit and preserves proposal refs.
func (s *Store) InitializeIdentity(ctx context.Context, allowAdoption bool) (IdentityResult, error) {
	return s.initializeIdentity(ctx, allowAdoption, (*sql.Tx).Commit)
}

func (s *Store) initializeIdentity(ctx context.Context, allowAdoption bool, finalize func(*sql.Tx) error) (result IdentityResult, err error) {
	s.proposalMu.Lock()
	defer s.proposalMu.Unlock()
	db, err := s.handle()
	if err != nil {
		return result, err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, conn.Close()) }()
	result.ProjectIdentity, err = s.checkProjectIdentity(ctx, conn, MainBranch, false)
	if err != nil || result.ProjectID != "" || s.cfg.Global {
		return result, err
	}
	expected, err := s.resolvedIdentity(ctx)
	if err != nil || expected.ProjectID == "" {
		return result, err
	}
	if !allowAdoption {
		return result, errors.New("existing store has no project identity; review Git origin, then run `memdolt init --adopt-identity --dir <repository>`; no identity was written")
	}
	if err := requireTransferClean(ctx, conn); err != nil {
		return result, err
	}
	head, err := branchHead(ctx, conn, MainBranch)
	if err != nil {
		return result, err
	}
	if err := validateTransferSchema(ctx, conn, head); err != nil {
		return result, err
	}
	commit, err := s.commitConnFinalize(ctx, conn, store.CommitRequest{
		Author: s.cfg.Actor, Message: "initialize repository project identity", RequireClean: true,
		Text:       []string{expected.ProjectID, expected.Origin},
		Statements: []store.Statement{{SQL: "INSERT INTO meta (k, v) VALUES (?, ?), (?, ?)", Args: []any{"project_id", expected.ProjectID, "project_origin", expected.Origin}}},
	}, finalize)
	if commit.Hash != "" {
		result.ProjectIdentity, result.Commit = expected, commit.Hash
		s.mu.Lock()
		s.identity = expected
		s.mu.Unlock()
	}
	return result, err
}
