package hub

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

type InitResult struct {
	Status string   `json:"status"`
	Output string   `json:"output"`
	Files  []string `json:"files"`
	Error  string   `json:"error,omitempty"`
}

// Init creates a new bundle in an existing parent. An exact existing bundle is
// a no-op; every other existing destination refuses without replacement. A
// failed new write may leave partial artifacts, identified by Output and Files.
func Init(output string, cfg Config) (result InitResult, err error) {
	result = InitResult{Status: "failed", Output: output, Files: []string{}}
	defer func() {
		if err != nil {
			result.Error = err.Error()
		}
	}()
	files, err := cfg.artifacts()
	if err != nil {
		return result, err
	}
	if err := localPath(output); err != nil {
		return result, err
	}
	parent, err := openDir(filepath.Dir(output), false)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, parent.Close()) }()
	name := filepath.Base(output)
	if _, err := parent.Lstat(name); err == nil {
		bundle, err := openDir(output, false)
		if err != nil {
			return result, err
		}
		err = errors.Join(compareFiles(bundle, files, false), bundle.Close())
		if err == nil {
			result.Status = "unchanged"
		}
		return result, err
	} else if !os.IsNotExist(err) {
		return result, err
	}
	if err := parent.Mkdir(name, 0o755); err != nil {
		return result, err
	}
	info, err := parent.Lstat(name)
	if err != nil || !info.IsDir() || unsafeLink(info) {
		return result, errors.Join(errors.New("new hub directory changed after creation"), err)
	}
	bundle, err := parent.OpenRoot(name)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, bundle.Close()) }()
	actual, err := bundle.Stat(".")
	if err != nil || !os.SameFile(info, actual) {
		return result, errors.Join(errors.New("new hub directory changed while opening"), err)
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		file, err := bundle.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return result, err
		}
		_, writeErr := file.Write(files[name])
		if err := errors.Join(writeErr, file.Sync(), file.Close()); err != nil {
			return result, err
		}
		result.Files = append(result.Files, filepath.Join(output, name))
	}
	result.Status = "written"
	return result, nil
}

func localPath(p string) error {
	if !filepath.IsAbs(p) || filepath.Clean(p) != p || filepath.Dir(p) == p ||
		strings.HasPrefix(filepath.VolumeName(p), `\\`) {
		return errors.New("hub output/config path must be a canonical absolute local path")
	}
	for _, part := range strings.FieldsFunc(p[len(filepath.VolumeName(p)):], func(r rune) bool { return r == '/' || r == '\\' }) {
		switch strings.ToLower(part) {
		case ".git", ".memdolt", ".memhub", ".orchestrator":
			return errors.New("hub output/config path collides with protected repository metadata")
		}
		if runtime.GOOS == "windows" && (strings.ContainsAny(part, `:<>"|?*`) || strings.TrimRight(part, " .") != part) {
			return errors.New("unsafe Windows hub path component")
		}
	}
	return nil
}

// Like the renderer's rooted walk, check each component and its opened identity.
// Hub deployment additionally requires root ownership for privileged consumers;
// changing the renderer's separate path contract would broaden this delivery.
func openDir(p string, secure bool) (*os.Root, error) {
	if err := localPath(p); err != nil {
		return nil, err
	}
	volume := filepath.VolumeName(p)
	root, err := os.OpenRoot(volume + string(filepath.Separator))
	if err != nil {
		return nil, err
	}
	for _, part := range strings.Split(strings.TrimPrefix(p[len(volume):], string(filepath.Separator)), string(filepath.Separator)) {
		info, err := root.Lstat(part)
		if err != nil || !info.IsDir() || unsafeLink(info) || (secure && !rootOwned(info)) {
			return nil, errors.Join(errors.New("hub directory must be unlinked; deployed paths must be root-owned and not group/world writable"), err, root.Close())
		}
		next, err := root.OpenRoot(part)
		if err != nil {
			return nil, errors.Join(err, root.Close())
		}
		actual, statErr := next.Stat(".")
		if err := errors.Join(statErr, root.Close()); err != nil || !os.SameFile(info, actual) {
			return nil, errors.Join(errors.New("hub directory changed while opening"), err, next.Close())
		}
		root = next
	}
	return root, nil
}

func readRegular(root *os.Root, name string, secure bool) (data []byte, err error) {
	info, err := root.Lstat(name)
	if err != nil || !info.Mode().IsRegular() || unsafeLink(info) || (secure && !rootOwned(info)) {
		return nil, errors.Join(fmt.Errorf("refuse missing, linked, nonregular or unsafe hub file %s", name), err)
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	actual, err := f.Stat()
	one, linkErr := singleLink(f)
	if err != nil || linkErr != nil || !one || !os.SameFile(info, actual) {
		return nil, errors.Join(errors.New("hub file identity or single-link check failed"), err, linkErr)
	}
	data, err = io.ReadAll(io.LimitReader(f, 1<<20+1))
	if len(data) > 1<<20 {
		return nil, errors.New("hub artifact exceeds one MiB")
	}
	return data, err
}

func compareFiles(root *os.Root, files map[string][]byte, secure bool) error {
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	names, readErr := dir.Readdirnames(-1)
	if err := errors.Join(readErr, dir.Close()); err != nil {
		return err
	}
	if len(names) != len(files) {
		return errors.New("hub bundle contains missing or foreign files; no replacement performed")
	}
	for _, name := range names {
		expected, exists := files[name]
		if !exists {
			return errors.New("hub bundle contains a foreign file; no replacement performed")
		}
		actual, err := readRegular(root, name, secure)
		if err != nil {
			return err
		}
		if !bytes.Equal(actual, expected) {
			return fmt.Errorf("hub artifact %s differs from the generated bundle; no replacement performed", name)
		}
	}
	return nil
}

func load(config string, deployed bool) (cfg Config, err error) {
	if err := localPath(config); err != nil {
		return cfg, err
	}
	if filepath.Base(config) != manifestName {
		return cfg, errors.New("hub --config must select the generated hub.json")
	}
	root, err := openDir(filepath.Dir(config), deployed)
	if err != nil {
		return cfg, err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	raw, err := readRegular(root, manifestName, deployed)
	if err != nil {
		return cfg, err
	}
	if json.Unmarshal(raw, &cfg) != nil {
		return cfg, errors.New("invalid hub JSON configuration")
	}
	files, err := cfg.artifacts()
	if err != nil {
		return cfg, err
	}
	// Exact canonical bytes also refuse unknown/duplicate/case-alias members,
	// invalid Unicode and nulls without a second custom JSON parser.
	if !bytes.Equal(raw, files[manifestName]) {
		return cfg, errors.New("hub.json must match its canonical generated nonsecret format")
	}
	if deployed && filepath.Dir(config) != cfg.ConfigDir {
		return cfg, errors.New("install the complete bundle at its configured absolute config_dir before checking deployment")
	}
	return cfg, compareFiles(root, files, deployed)
}
