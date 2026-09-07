package render

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/kninetimmy/memdolt/internal/layout"
)

var filenames = [2]string{"PROJECT.md", "PROJECT_LEDGER.md"}

const lockName = ".memdolt-render.lock"

type config struct {
	base, output, backup, project string
	staleDays                     int64
}

func loadConfig(baseDir string) (cfg config, err error) {
	base, err := filepath.Abs(baseDir)
	if err != nil {
		return cfg, err
	}
	// The explicitly selected repository may itself be an alias. Resolve that
	// root once; managed descendants and output routing never follow links.
	cfg.base, err = filepath.EvalSymlinks(base)
	if err != nil {
		return cfg, err
	}
	paths, err := layout.New(cfg.base)
	if err != nil {
		return cfg, err
	}
	metadata, err := openDir(paths.Dir(), false)
	if err != nil {
		return cfg, fmt.Errorf("open repository configuration directory: %w", err)
	}
	defer func() { err = errors.Join(err, metadata.Close()) }()
	file := struct {
		ProjectName string `toml:"project_name"`
		Render      struct {
			OutputDir string `toml:"output_dir"`
		} `toml:"render"`
		Retrieval struct {
			StaleDays int64 `toml:"fact_stale_after_days"`
		} `toml:"retrieval"`
	}{}
	file.ProjectName = filepath.Base(cfg.base)
	file.Render.OutputDir = filepath.Join(layout.DirName, "rendered")
	file.Retrieval.StaleDays = 90 // memhub parity, also retrieval's default
	info, err := regular(metadata, layout.ConfigFileName)
	if err != nil {
		return cfg, err
	}
	if info != nil {
		raw, err := readFile(metadata, layout.ConfigFileName, info)
		if err != nil {
			return cfg, err
		}
		decoded, err := toml.Decode(string(raw), &file)
		if err != nil {
			return cfg, fmt.Errorf("read render configuration: %w", err)
		}
		for _, key := range decoded.Undecoded() {
			if len(key) > 0 && strings.EqualFold(key[0], "render") {
				return cfg, fmt.Errorf("unknown render configuration key %s", key)
			}
		}
	}
	if strings.TrimSpace(file.Render.OutputDir) == "" || file.Retrieval.StaleDays < 1 {
		return cfg, errors.New("render.output_dir must be nonempty and retrieval.fact_stale_after_days must be positive")
	}
	cfg.output = file.Render.OutputDir
	if !filepath.IsAbs(cfg.output) {
		if !filepath.IsLocal(cfg.output) {
			return cfg, errors.New("relative render.output_dir must stay within the repository")
		}
		cfg.output = filepath.Join(cfg.base, cfg.output)
	}
	cfg.output = filepath.Clean(cfg.output)
	cfg.backup = filepath.Join(paths.Dir(), "backups", "rendered")
	cfg.project, cfg.staleDays = file.ProjectName, file.Retrieval.StaleDays
	if err := checkOutput(cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func checkOutput(cfg config) error {
	rel, err := filepath.Rel(cfg.base, cfg.output)
	if err != nil || !filepath.IsLocal(rel) {
		rel = cfg.output
	}
	parts := strings.FieldsFunc(rel, func(r rune) bool { return r == '/' || r == '\\' })
	for i, part := range parts {
		switch strings.ToLower(part) {
		case ".git", ".memhub", ".orchestrator":
			return errors.New("render.output_dir collides with protected repository metadata")
		case layout.DirName:
			allowed := filepath.Join(cfg.base, layout.DirName, "rendered")
			under, err := filepath.Rel(allowed, cfg.output)
			if err != nil || !filepath.IsLocal(under) || i+1 >= len(parts) || parts[i+1] != "rendered" {
				return errors.New("render.output_dir may use .memdolt/rendered only; store, config, backups and derived indexes are protected")
			}
		}
	}
	return nil
}

// openDir walks from the volume root with directory handles, checking each
// component before opening it and checking its identity afterward. All later
// operations stay relative to the reached directory handles. No path-based
// MkdirAll/rename can follow a swapped ancestor out of the intended root.
func openDir(path string, create bool) (*os.Root, error) {
	volume := filepath.VolumeName(path)
	if !filepath.IsAbs(path) || strings.HasPrefix(volume, `\\`) {
		return nil, errors.New("render paths must be absolute local filesystem paths; network/device namespaces are unsupported")
	}
	root, err := os.OpenRoot(volume + string(filepath.Separator))
	if err != nil {
		return nil, err
	}
	for _, part := range strings.Split(strings.TrimPrefix(path[len(volume):], string(filepath.Separator)), string(filepath.Separator)) {
		if part == "" {
			continue
		}
		if part == "." || part == ".." || (runtime.GOOS == "windows" &&
			(strings.ContainsAny(part, `:<>"|?*`) || strings.TrimRight(part, " .") != part)) {
			return nil, errors.Join(errors.New("unsafe render path component"), root.Close())
		}
		info, statErr := root.Lstat(part)
		if os.IsNotExist(statErr) && create {
			if mkdirErr := root.Mkdir(part, 0o700); mkdirErr != nil && !os.IsExist(mkdirErr) {
				return nil, errors.Join(mkdirErr, root.Close())
			}
			info, statErr = root.Lstat(part)
		}
		if statErr != nil {
			return nil, errors.Join(statErr, root.Close())
		}
		if !info.IsDir() || unsafeLink(info) {
			return nil, errors.Join(fmt.Errorf("render path %s contains a symlink, reparse point or non-directory", path), root.Close())
		}
		next, openErr := root.OpenRoot(part)
		if openErr != nil {
			return nil, errors.Join(openErr, root.Close())
		}
		actual, statErr := next.Stat(".")
		closeErr := root.Close()
		if statErr != nil || closeErr != nil || !os.SameFile(info, actual) {
			return nil, errors.Join(errors.New("render directory changed while opening"), statErr, closeErr, next.Close())
		}
		root = next
	}
	return root, nil
}

func regular(root *os.Root, name string) (os.FileInfo, error) {
	info, err := root.Lstat(name)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || unsafeLink(info) {
		return nil, fmt.Errorf("refuse non-regular, symlink or reparse file %s", filepath.Join(root.Name(), name))
	}
	return info, nil
}

func readFile(root *os.Root, name string, expected os.FileInfo) (data []byte, err error) {
	file, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	actual, err := file.Stat()
	if err != nil || !os.SameFile(expected, actual) {
		return nil, errors.Join(errors.New("render input changed while opening"), err)
	}
	return io.ReadAll(file)
}

// fileHooks inject only failures otherwise inaccessible to portable fixtures.
type fileHooks struct {
	beforePrepare func(int) error
	beforeBackup  func(int) error
	beforeReplace func(int) error
	finalize      func() error
}

func writeFiles(ctx context.Context, cfg config, contents [2]string, result *Result, hooks fileHooks) (err error) {
	out, err := openDir(cfg.output, true)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, out.Close()) }()
	// A root-relative exclusive create also coordinates different repository
	// owners configured to use the same output. It has no PID/staleness guesses.
	// Crash residue requires inspection with all renders stopped before removal.
	lock, err := out.OpenFile(lockName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("output generation is locked or unavailable; if %s remains after a crash, stop all renders and inspect outputs/backups before removing that lock: %w", filepath.Join(cfg.output, lockName), err)
	}
	lockInfo, statErr := lock.Stat()
	closeErr := lock.Close()
	if statErr != nil || closeErr != nil {
		return errors.Join(statErr, closeErr)
	}
	defer func() { err = errors.Join(err, removeOwned(out, lockName, lockInfo)) }()
	backup, err := openDir(cfg.backup, true)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, backup.Close()) }()
	type prepared struct {
		name, temp string
		original   os.FileInfo
		tempInfo   os.FileInfo
		bytes      []byte
	}
	items := make([]prepared, 0, len(filenames))
	defer func() {
		for _, item := range items {
			if item.temp != "" {
				err = errors.Join(err, removeOwned(out, item.temp, item.tempInfo))
			}
		}
	}()
	for i, name := range filenames {
		if err := ctx.Err(); err != nil {
			return err
		}
		if hooks.beforePrepare != nil {
			if err := hooks.beforePrepare(i); err != nil {
				return err
			}
		}
		original, err := regular(out, name)
		if err != nil {
			return err
		}
		var old []byte
		if original != nil {
			old, err = readFile(out, name, original)
			if err != nil {
				return err
			}
			if !strings.HasPrefix(string(old), Marker+"\n") {
				return fmt.Errorf("refuse to overwrite unmarked user file %s; choose an unused render.output_dir", filepath.Join(cfg.output, name))
			}
			if hooks.beforeBackup != nil {
				if err := hooks.beforeBackup(i); err != nil {
					return err
				}
			}
			backupName, _, err := createFile(backup, name+".bak-", old)
			if err != nil {
				return fmt.Errorf("prepare backup of %s: %w", name, err)
			}
			result.BackupFiles = append(result.BackupFiles, filepath.Join(cfg.backup, backupName))
		}
		temp, info, err := createFile(out, "."+name+".tmp-", []byte(contents[i]))
		if err != nil {
			return fmt.Errorf("prepare %s: %w", name, err)
		}
		items = append(items, prepared{name: name, temp: temp, original: original, tempInfo: info, bytes: old})
	}
	result.Status = "prepared"
	for i := range items {
		item := &items[i]
		if err := ctx.Err(); err != nil {
			return err
		}
		if hooks.beforeReplace != nil {
			if err := hooks.beforeReplace(i); err != nil {
				return err
			}
		}
		current, err := regular(out, item.name)
		if err != nil {
			return err
		}
		if (current == nil) != (item.original == nil) || (current != nil && !os.SameFile(current, item.original)) {
			return fmt.Errorf("destination %s changed after preparation; inspect before retrying", item.name)
		}
		if current != nil {
			data, err := readFile(out, item.name, current)
			if err != nil || string(data) != string(item.bytes) {
				return errors.Join(fmt.Errorf("destination %s changed after backup", item.name), err)
			}
		}
		// Root.Rename uses the platform's replacement operation on sibling files.
		// No truncation, pair-wide atomicity or stronger crash guarantee is claimed.
		if err := out.Rename(item.temp, item.name); err != nil {
			return fmt.Errorf("replace %s: %w", item.name, err)
		}
		item.temp = ""
		result.WrittenFiles = append(result.WrittenFiles, filepath.Join(cfg.output, item.name))
		result.Status = "partial"
	}
	result.Status = "written"
	if hooks.finalize != nil {
		return hooks.finalize()
	}
	return nil
}

func createFile(root *os.Root, prefix string, content []byte) (name string, info os.FileInfo, err error) {
	name = prefix + rand.Text()
	file, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", nil, err
	}
	info, err = file.Stat()
	if err == nil {
		_, err = file.Write(content)
	}
	if err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		err = errors.Join(err, removeOwned(root, name, info))
	}
	return name, info, err
}

func removeOwned(root *os.Root, name string, expected os.FileInfo) error {
	actual, err := root.Lstat(name)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil || expected == nil || !os.SameFile(expected, actual) {
		return errors.Join(fmt.Errorf("retain changed/unverifiable render artifact %s", filepath.Join(root.Name(), name)), err)
	}
	if err := root.Remove(name); err != nil {
		return fmt.Errorf("remove render temporary artifact %s: %w", filepath.Join(root.Name(), name), err)
	}
	return nil
}
