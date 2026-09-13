package codeindex

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxGitCommits = 100000
	maxGitBytes   = 64 << 20
)

type GitRange struct {
	Head       string    `json:"head"`
	Since      *string   `json:"since"`
	Shallow    bool      `json:"shallow"`
	ObservedAt time.Time `json:"observedAt"`
}

type GitIngestSummary struct {
	GitRange
	Committed           bool `json:"committed"`
	OutcomeUnknown      bool `json:"outcomeUnknown"`
	CommitsSeen         int  `json:"commitsSeen"`
	CommitsIndexed      int  `json:"commitsIndexed"`
	UniqueFilesSeen     int  `json:"uniqueFilesSeen"`
	CommitFileLinksSeen int  `json:"commitFileLinksSeen"`
	DeniedCommits       int  `json:"deniedCommits"`
	DeniedFilesSkipped  int  `json:"deniedFilesSkipped"`
}

type gitCommit struct {
	SHA, Author, Subject string
	AuthoredAt           string
	AuthoredUnix         int64
	Files                []gitChange
}

type gitChange struct {
	Path, Source, Type string
}

// IngestGit observes only local committed Git objects and publishes one complete
// range in the derived index. It does not read source bodies or use inference.
func IngestGit(ctx context.Context, start string, since *string) (summary GitIngestSummary, err error) {
	if since != nil && (strings.TrimSpace(*since) == "" || !utf8.ValidString(*since) || strings.ContainsRune(*since, 0)) {
		return summary, errors.New("--since requires a nonempty UTF-8 commit-ish")
	}
	r, err := openRepository(start, true)
	if err != nil {
		return summary, err
	}
	defer func() { err = errors.Join(err, r.close()) }()
	if err := r.acquire(); err != nil {
		return summary, err
	}
	// Refuse foreign cache state before running Git, but defer schema writes until
	// every Git observation and protection check has succeeded.
	db, _, err := r.openDB(ctx, false)
	if err != nil {
		return summary, err
	}
	if db != nil {
		err = errors.Join(checkOwnedSchema(ctx, db), db.Close())
		if err != nil {
			return summary, err
		}
	}
	selected, commits, err := loadGitHistory(ctx, r.base, since)
	if err != nil {
		return summary, err
	}
	if r.cfg.denied.Check(selected.Head, selected.ObservedAt.Format(gitObservedFormat)) != nil || selected.Since != nil && r.cfg.denied.Check(*selected.Since) != nil {
		return summary, errors.New("selected Git revision matches a deny-list rule")
	}
	summary.GitRange, summary.CommitsSeen = selected, len(commits)
	allowed, files := make([]gitCommit, 0, len(commits)), map[string]bool{}
	checked := map[string]bool{}
	for _, commit := range commits {
		if r.cfg.denied.Check(commit.SHA, commit.Author, commit.AuthoredAt, commit.Subject) != nil {
			summary.DeniedCommits++
			summary.DeniedFilesSkipped += len(commit.Files)
			continue
		}
		changes := make([]gitChange, 0, len(commit.Files))
		for _, change := range commit.Files {
			denied := r.cfg.denied.Check(change.Type) != nil
			for _, path := range []string{change.Path, change.Source} {
				if path == "" {
					continue
				}
				ok, seen := checked[path]
				if !seen {
					ok, err = r.historyPathAllowed(path)
					if err != nil {
						return summary, err
					}
					checked[path] = ok
				}
				denied = denied || !ok
			}
			if denied {
				summary.DeniedFilesSkipped++
				continue
			}
			changes = append(changes, change)
			files[change.Path] = true
			summary.CommitFileLinksSeen++
		}
		commit.Files = changes
		allowed = append(allowed, commit)
	}
	summary.CommitsIndexed, summary.UniqueFilesSeen = len(allowed), len(files)
	db, created, err := r.openDB(ctx, true)
	if err != nil {
		return summary, err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	if err := bootstrap(ctx, db, created); err != nil {
		return summary, err
	}
	return publishGitHistory(ctx, db, summary, allowed, (*sql.Tx).Commit)
}

func publishGitHistory(ctx context.Context, db *sql.DB, summary GitIngestSummary, commits []gitCommit, finalize func(*sql.Tx) error) (_ GitIngestSummary, err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return summary, err
	}
	defer rollback(tx, &err)
	for _, commit := range commits {
		if _, err := tx.ExecContext(ctx, `INSERT INTO git_commits(sha,author,authored_at,authored_unix,subject) VALUES (?,?,?,?,?)
ON CONFLICT(sha) DO UPDATE SET author=excluded.author,authored_at=excluded.authored_at,authored_unix=excluded.authored_unix,subject=excluded.subject`,
			commit.SHA, commit.Author, commit.AuthoredAt, commit.AuthoredUnix, commit.Subject); err != nil {
			return summary, err
		}
		for _, change := range commit.Files {
			if _, err := tx.ExecContext(ctx, "INSERT INTO git_files(path) VALUES (?) ON CONFLICT(path) DO NOTHING", change.Path); err != nil {
				return summary, err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO git_commit_files(path,commit_sha,change_type) VALUES (?,?,?)
ON CONFLICT(path,commit_sha) DO UPDATE SET change_type=excluded.change_type`, change.Path, commit.SHA, change.Type); err != nil {
				return summary, err
			}
		}
	}
	since := ""
	if summary.Since != nil {
		since = *summary.Since
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO git_ingestions(head,since_commit,shallow,observed_at) VALUES (?,?,?,?)
ON CONFLICT(head,since_commit) DO UPDATE SET shallow=excluded.shallow,observed_at=excluded.observed_at`, summary.Head, since, summary.Shallow, summary.ObservedAt.UTC().Format(gitObservedFormat)); err != nil {
		return summary, err
	}
	if err := finalize(tx); err != nil {
		summary.OutcomeUnknown = true
		return summary, fmt.Errorf("git-history transaction finalization was not confirmed; inspect the cache before assuming success: %w", err)
	}
	summary.Committed = true
	return summary, nil
}

// ponytail: bounded in-memory observations, at most 100000 commits and 64 MiB
// per Git response; stream into staging only if real repository sizes need it.
func loadGitHistory(ctx context.Context, base string, since *string) (selected GitRange, commits []gitCommit, err error) {
	selected.Head, err = gitRevision(ctx, base, "HEAD")
	if err != nil {
		return selected, nil, err
	}
	selection := selected.Head
	if since != nil {
		id, err := gitRevision(ctx, base, *since)
		if err != nil {
			return selected, nil, err
		}
		selected.Since = &id
		selection = id + ".." + selected.Head
	}
	shallow, err := gitOutput(ctx, base, "", "rev-parse", "--is-shallow-repository")
	if err != nil {
		return selected, nil, err
	}
	switch string(shallow) {
	case "true\n":
		selected.Shallow = true
	case "false\n":
	default:
		return selected, nil, errors.New("invalid Git shallow-repository observation")
	}
	ids, err := gitOutput(ctx, base, "", "rev-list", "--topo-order", "--max-count=100001", selection, "--")
	if err != nil {
		return selected, nil, err
	}
	var hashes []string
	seen := map[string]bool{}
	if len(ids) > 0 {
		if ids[len(ids)-1] != '\n' {
			return selected, nil, errors.New("incomplete Git revision list")
		}
		hashes = strings.Split(string(ids[:len(ids)-1]), "\n")
	}
	if len(hashes) > maxGitCommits {
		return selected, nil, errors.New("git range exceeds 100000 commits; select a smaller --since range")
	}
	for _, hash := range hashes {
		if !gitOID(hash) || seen[hash] {
			return selected, nil, errors.New("invalid Git revision list")
		}
		seen[hash] = true
	}
	if selected.Since == nil && !seen[selected.Head] {
		return selected, nil, errors.New("git revision list did not observe captured HEAD")
	}
	if len(hashes) > 0 {
		objects, err := gitOutput(ctx, base, string(ids), "cat-file", "--batch")
		if err != nil {
			return selected, nil, err
		}
		if err := validateGitObjects(objects, hashes); err != nil {
			return selected, nil, err
		}
		metadata, err := gitOutput(ctx, base, string(ids), "log", "--no-walk=unsorted", "--stdin", "-z", "--encoding=none", "--format=%H%x00%an%x00%aI%x00%s")
		if err != nil {
			return selected, nil, err
		}
		commits, err = parseGitMetadata(metadata, hashes)
		if err != nil {
			return selected, nil, err
		}
		changes, err := gitOutput(ctx, base, string(ids), "diff-tree", "--stdin", "--root", "--always", "-r", "-z", "--name-status", "--diff-merges=first-parent", "--find-renames=50%", "--find-copies=50%", "-l1000", "--no-ext-diff", "--no-textconv", "--ignore-submodules=none")
		if err != nil {
			return selected, nil, err
		}
		if err := parseGitChanges(changes, commits); err != nil {
			return selected, nil, err
		}
	}
	selected.ObservedAt = time.Now().UTC()
	return selected, commits, nil
}

func gitOID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	return !strings.ContainsFunc(value, func(r rune) bool { return r < '0' || r > '9' && r < 'a' || r > 'f' })
}

func gitRevision(ctx context.Context, base, revision string) (string, error) {
	out, err := gitOutput(ctx, base, "", "rev-parse", "--verify", "--end-of-options", revision+"^{commit}")
	if err != nil {
		return "", errors.Join(errors.New("git commit revision is invalid or unavailable locally"), err)
	}
	id := strings.TrimSuffix(string(out), "\n")
	if !gitOID(id) {
		return "", errors.New("invalid Git commit revision observation")
	}
	return id, nil
}

type gitBuffer struct {
	bytes.Buffer
	cancel context.CancelFunc
}

func (b *gitBuffer) Write(p []byte) (int, error) {
	if len(p) > maxGitBytes-b.Len() {
		b.cancel()
		return 0, errors.New("git observation exceeds 64 MiB; use a smaller --since range")
	}
	return b.Buffer.Write(p)
}

func gitOutput(ctx context.Context, base, input string, args ...string) ([]byte, error) {
	limited, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(limited, "git", append([]string{"--no-replace-objects", "-C", base, "-c", "protocol.allow=never"}, args...)...)
	// Caller Git environment must not select a different repository, inject
	// config, substitute objects, or trigger lazy network fetches.
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(entry), "GIT_") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_NO_LAZY_FETCH=1", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_GRAFT_FILE="+os.DevNull)
	cmd.Stdin = strings.NewReader(input)
	cmd.Stderr = io.Discard // Git diagnostics may contain denied paths or prose.
	output := &gitBuffer{cancel: cancel}
	cmd.Stdout = output
	if err := cmd.Run(); err != nil {
		if limited.Err() != nil && ctx.Err() == nil {
			return nil, errors.New("git observation exceeds 64 MiB; use a smaller --since range")
		}
		return nil, fmt.Errorf("git %s observation failed: %w", args[0], errors.Join(err, ctx.Err()))
	}
	return output.Bytes(), nil
}

// cat-file length frames validate the original bytes before pretty-format's
// NUL frames are trusted. Git otherwise tolerates/truncates malformed objects
// or transcodes a declared legacy encoding, hiding invalid input from a parser.
func validateGitObjects(raw []byte, hashes []string) error {
	for _, hash := range hashes {
		header, rest, ok := bytes.Cut(raw, []byte{'\n'})
		fields := strings.Split(string(header), " ")
		if !ok || len(fields) != 3 || fields[0] != hash || fields[1] != "commit" {
			return errors.New("invalid or unavailable Git commit object")
		}
		size, err := strconv.Atoi(fields[2])
		if err != nil || size < 0 || size >= len(rest) || rest[size] != '\n' {
			return errors.New("incomplete Git commit object")
		}
		object := rest[:size]
		if !utf8.Valid(object) || bytes.IndexByte(object, 0) >= 0 {
			return errors.New("git commit objects must be UTF-8 without NUL bytes")
		}
		commitHeader, _, ok := bytes.Cut(object, []byte("\n\n"))
		if !ok {
			return errors.New("malformed Git commit headers")
		}
		counts := map[string]int{}
		for _, line := range strings.Split(string(commitHeader), "\n") {
			key, value, ok := strings.Cut(line, " ")
			if !ok {
				return errors.New("malformed Git commit header")
			}
			counts[key]++
			if (key == "tree" || key == "parent") && !gitOID(value) {
				return errors.New("malformed Git tree or parent identity")
			}
			if (key == "author" || key == "committer") && !validGitIdentity(value) {
				return errors.New("malformed Git author or committer identity/date")
			}
			if key == "encoding" && !strings.EqualFold(value, "UTF-8") && !strings.EqualFold(value, "UTF8") {
				return errors.New("unsupported Git commit encoding; only UTF-8 is accepted")
			}
		}
		if counts["tree"] != 1 || counts["author"] != 1 || counts["committer"] != 1 || counts["encoding"] > 1 {
			return errors.New("malformed Git commit headers")
		}
		raw = rest[size+1:]
	}
	if len(raw) != 0 {
		return errors.New("unexpected trailing Git object data")
	}
	return nil
}

func validGitIdentity(value string) bool {
	name := strings.LastIndex(value, " <")
	end := strings.LastIndex(value, "> ")
	if name < 1 || end <= name+2 || strings.ContainsRune(value, '\r') {
		return false
	}
	fields := strings.Split(value[end+2:], " ")
	if len(fields) != 2 || len(fields[1]) != 5 || fields[1][0] != '+' && fields[1][0] != '-' {
		return false
	}
	_, secondsErr := strconv.ParseInt(fields[0], 10, 64)
	offset, offsetErr := strconv.Atoi(fields[1][1:])
	return secondsErr == nil && offsetErr == nil && offset >= 0 && offset/100 < 24 && offset%100 < 60
}

func parseGitMetadata(raw []byte, hashes []string) ([]gitCommit, error) {
	if !utf8.Valid(raw) {
		return nil, errors.New("git metadata must be UTF-8")
	}
	fields := strings.Split(string(raw), "\x00")
	if len(fields) != len(hashes)*4+1 || fields[len(fields)-1] != "" {
		return nil, errors.New("malformed Git metadata framing")
	}
	commits := make([]gitCommit, len(hashes))
	for i, hash := range hashes {
		row := fields[i*4 : i*4+4]
		when, err := time.Parse(time.RFC3339, row[2])
		if err != nil || row[0] != hash || row[1] == "" {
			return nil, errors.New("invalid Git commit identity, author or author date")
		}
		commits[i] = gitCommit{SHA: hash, Author: row[1], AuthoredAt: row[2], AuthoredUnix: when.Unix(), Subject: row[3]}
	}
	return commits, nil
}

func parseGitChanges(raw []byte, commits []gitCommit) error {
	if !utf8.Valid(raw) {
		return errors.New("git history paths must be UTF-8")
	}
	fields := strings.Split(string(raw), "\x00")
	if len(fields) == 0 || fields[len(fields)-1] != "" {
		return errors.New("incomplete Git file-change framing")
	}
	fields = fields[:len(fields)-1]
	for i := range commits {
		if len(fields) == 0 || fields[0] != commits[i].SHA {
			return errors.New("git file changes did not observe every selected commit")
		}
		fields = fields[1:]
		seen := map[string]bool{}
		for len(fields) > 0 && !gitOID(fields[0]) {
			status := fields[0]
			fields = fields[1:]
			if status == "" || !strings.ContainsRune("ADMTRC", rune(status[0])) {
				return errors.New("unsupported Git file change type")
			}
			change := gitChange{Type: status[:1]}
			if change.Type == "R" || change.Type == "C" {
				score, err := strconv.Atoi(status[1:])
				if err != nil || score < 0 || score > 100 || len(fields) < 2 || fields[0] == "" {
					return errors.New("malformed Git rename/copy record")
				}
				change.Source, fields = fields[0], fields[1:]
			} else if len(status) != 1 {
				return errors.New("unsupported Git file change status")
			}
			if len(fields) == 0 || fields[0] == "" || seen[fields[0]] {
				return errors.New("missing or duplicate Git file change path")
			}
			change.Path, fields = fields[0], fields[1:]
			seen[change.Path] = true
			commits[i].Files = append(commits[i].Files, change)
		}
	}
	if len(fields) != 0 {
		return errors.New("unexpected trailing Git file-change data")
	}
	return nil
}
