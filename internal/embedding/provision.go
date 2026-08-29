package embedding

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/kninetimmy/memdolt/models"
)

const maxArtifactBytes = int64(512 << 20)

// Options controls artifact provisioning for Open.
type Options struct {
	// CacheDir defaults to ~/.memdolt/models. For offline use, pre-position
	// every models/manifest.json local path beneath this directory and set
	// Offline. Every pre-positioned file is still SHA-256 verified.
	CacheDir string
	// RuntimePath optionally points at a pre-positioned ONNX Runtime shared
	// library outside CacheDir. It must match the current platform's pin.
	RuntimePath string
	// Offline forbids network fetches. Missing artifacts fail with the exact
	// path and upstream URL needed to pre-position them.
	Offline bool
	// HTTPClient is optional. The default has a 30-minute whole-request
	// timeout so a stalled first-run model fetch eventually fails.
	HTTPClient *http.Client
}

type modelFilePin struct {
	RemotePath string `json:"remote_path"`
	LocalName  string `json:"local_name"`
	SHA256     string `json:"sha256"`
}

type modelBundlePin struct {
	Name     string         `json:"name"`
	Revision string         `json:"revision"`
	BaseURL  string         `json:"base_url"`
	Files    []modelFilePin `json:"files"`
}

type runtimePin struct {
	GOOS          string `json:"goos"`
	GOARCH        string `json:"goarch"`
	LocalPath     string `json:"local_path"`
	SHA256        string `json:"sha256"`
	URL           string `json:"url"`
	ArchiveFormat string `json:"archive_format"`
	ArchiveMember string `json:"archive_member"`
	ArchiveSHA256 string `json:"archive_sha256"`
}

type artifactManifest struct {
	Version            int              `json:"version"`
	ONNXRuntimeVersion string           `json:"onnxruntime_version"`
	Bundles            []modelBundlePin `json:"bundles"`
	Runtimes           []runtimePin     `json:"runtimes"`
}

type provisionedArtifacts struct {
	models      map[string]map[string][]byte
	runtimePath string
}

type provisioner struct {
	manifest    artifactManifest
	cacheDir    string
	runtimePath string
	offline     bool
	client      *http.Client
	goos        string
	goarch      string
}

// DefaultCacheDir returns PRD §8.3's user-local model cache path.
func DefaultCacheDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locating home directory for model cache: %w", err)
	}
	return filepath.Join(home, ".memdolt", "models"), nil
}

func newProvisioner(opts Options) (*provisioner, error) {
	m, err := loadManifest(models.ManifestJSON())
	if err != nil {
		return nil, err
	}
	cacheDir := opts.CacheDir
	if cacheDir == "" {
		cacheDir, err = DefaultCacheDir()
		if err != nil {
			return nil, err
		}
	}
	cacheDir, err = filepath.Abs(cacheDir)
	if err != nil {
		return nil, fmt.Errorf("resolving model cache %s: %w", cacheDir, err)
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Minute}
	}
	return &provisioner{
		manifest:    m,
		cacheDir:    cacheDir,
		runtimePath: opts.RuntimePath,
		offline:     opts.Offline,
		client:      client,
		goos:        runtime.GOOS,
		goarch:      runtime.GOARCH,
	}, nil
}

func loadManifest(raw []byte) (artifactManifest, error) {
	var m artifactManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return artifactManifest{}, fmt.Errorf("parsing embedded models/manifest.json: %w", err)
	}
	if err := m.validate(); err != nil {
		return artifactManifest{}, fmt.Errorf("invalid embedded models/manifest.json: %w", err)
	}
	return m, nil
}

func (m artifactManifest) validate() error {
	if m.Version != 1 {
		return fmt.Errorf("version is %d, want 1", m.Version)
	}
	if m.ONNXRuntimeVersion == "" {
		return fmt.Errorf("onnxruntime_version is empty")
	}
	if len(m.Bundles) == 0 || len(m.Runtimes) == 0 {
		return fmt.Errorf("model bundles and runtimes must both be nonempty")
	}
	seenBundles := make(map[string]bool, len(m.Bundles))
	for _, bundle := range m.Bundles {
		if !safeName(bundle.Name) || seenBundles[bundle.Name] {
			return fmt.Errorf("invalid or duplicate model bundle name %q", bundle.Name)
		}
		seenBundles[bundle.Name] = true
		if bundle.Revision == "" || len(bundle.Files) == 0 {
			return fmt.Errorf("bundle %q has no revision or files", bundle.Name)
		}
		if err := validateHTTPSURL(bundle.BaseURL); err != nil {
			return fmt.Errorf("bundle %q base_url: %w", bundle.Name, err)
		}
		seenFiles := make(map[string]bool, len(bundle.Files))
		for _, file := range bundle.Files {
			if !safeName(file.LocalName) || seenFiles[file.LocalName] {
				return fmt.Errorf("bundle %q has invalid or duplicate local_name %q", bundle.Name, file.LocalName)
			}
			seenFiles[file.LocalName] = true
			if file.RemotePath == "" || strings.Contains(file.RemotePath, "..") {
				return fmt.Errorf("bundle %q has unsafe remote_path %q", bundle.Name, file.RemotePath)
			}
			if err := validateSHA256(file.SHA256); err != nil {
				return fmt.Errorf("bundle %q file %q: %w", bundle.Name, file.LocalName, err)
			}
		}
	}
	seenRuntimes := make(map[string]bool, len(m.Runtimes))
	for _, pin := range m.Runtimes {
		platform := pin.GOOS + "/" + pin.GOARCH
		if pin.GOOS == "" || pin.GOARCH == "" || seenRuntimes[platform] {
			return fmt.Errorf("invalid or duplicate runtime platform %q", platform)
		}
		seenRuntimes[platform] = true
		if !safeRelativePath(pin.LocalPath) {
			return fmt.Errorf("runtime %s has unsafe local_path %q", platform, pin.LocalPath)
		}
		if err := validateSHA256(pin.SHA256); err != nil {
			return fmt.Errorf("runtime %s file: %w", platform, err)
		}
		if err := validateSHA256(pin.ArchiveSHA256); err != nil {
			return fmt.Errorf("runtime %s archive: %w", platform, err)
		}
		if err := validateHTTPSURL(pin.URL); err != nil {
			return fmt.Errorf("runtime %s url: %w", platform, err)
		}
		if pin.ArchiveFormat != "zip" && pin.ArchiveFormat != "tar.gz" {
			return fmt.Errorf("runtime %s has unsupported archive format %q", platform, pin.ArchiveFormat)
		}
		if normalizeArchivePath(pin.ArchiveMember) == "" {
			return fmt.Errorf("runtime %s archive_member is empty", platform)
		}
	}
	return nil
}

func validateSHA256(value string) error {
	if len(value) != sha256.Size*2 {
		return fmt.Errorf("sha256 %q is not 64 hexadecimal characters", value)
	}
	if _, err := hex.DecodeString(value); err != nil {
		return fmt.Errorf("sha256 %q is not hexadecimal: %w", value, err)
	}
	return nil
}

func validateHTTPSURL(value string) error {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("%q is not an absolute HTTPS URL", value)
	}
	return nil
}

func safeName(name string) bool {
	return name != "" && filepath.Base(name) == name && !strings.ContainsAny(name, `/\\`)
}

func safeRelativePath(path string) bool {
	path = filepath.FromSlash(path)
	if path == "" || filepath.IsAbs(path) {
		return false
	}
	clean := filepath.Clean(path)
	return clean != "." && clean != ".." && !strings.HasPrefix(clean, ".."+string(filepath.Separator))
}

func (p *provisioner) provision(ctx context.Context) (*provisionedArtifacts, error) {
	runtimePath, err := p.ensureRuntime(ctx)
	if err != nil {
		return nil, err
	}
	bundles := make(map[string]map[string][]byte, len(p.manifest.Bundles))
	for _, bundle := range p.manifest.Bundles {
		files := make(map[string][]byte, len(bundle.Files))
		for _, pin := range bundle.Files {
			target, err := safeCachePath(p.cacheDir, filepath.Join(bundle.Name, pin.LocalName))
			if err != nil {
				return nil, err
			}
			upstream := strings.TrimRight(bundle.BaseURL, "/") + "/" + strings.TrimLeft(pin.RemotePath, "/")
			data, err := p.ensureDirectFile(ctx, target, upstream, pin.SHA256)
			if err != nil {
				return nil, fmt.Errorf("model %s/%s: %w", bundle.Name, pin.LocalName, err)
			}
			files[pin.LocalName] = data
		}
		bundles[bundle.Name] = files
	}
	return &provisionedArtifacts{models: bundles, runtimePath: runtimePath}, nil
}

func (p *provisioner) runtimePin() (runtimePin, error) {
	for _, pin := range p.manifest.Runtimes {
		if pin.GOOS == p.goos && pin.GOARCH == p.goarch {
			return pin, nil
		}
	}
	return runtimePin{}, fmt.Errorf("unsupported inference client platform %s/%s", p.goos, p.goarch)
}

func (p *provisioner) ensureRuntime(ctx context.Context) (string, error) {
	pin, err := p.runtimePin()
	if err != nil {
		return "", err
	}
	target := p.runtimePath
	if target != "" {
		target, err = canonicalPath(target)
	} else {
		target, err = safeCachePath(p.cacheDir, filepath.FromSlash(pin.LocalPath))
	}
	if err != nil {
		return "", fmt.Errorf("resolving ONNX Runtime path: %w", err)
	}
	if err := verifyFile(target, pin.SHA256); err == nil {
		return target, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("ONNX Runtime %s: %w", target, err)
	}
	if p.runtimePath != "" {
		return "", fmt.Errorf("ONNX Runtime override %s is missing; expected sha256=%s", target, pin.SHA256)
	}
	if p.offline {
		return "", fmt.Errorf("ONNX Runtime %s is missing in offline mode; fetch %s, verify archive sha256=%s, and pre-position the extracted %s with sha256=%s", target, pin.URL, pin.ArchiveSHA256, pin.ArchiveMember, pin.SHA256)
	}
	parent := filepath.Dir(target)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", fmt.Errorf("creating runtime cache directory %s: %w", parent, err)
	}
	archivePath, err := p.fetchVerified(ctx, pin.URL, parent, ".runtime-archive-*", pin.ArchiveSHA256)
	if err != nil {
		return "", fmt.Errorf("fetching ONNX Runtime archive: %w", err)
	}
	defer func() { _ = os.Remove(archivePath) }()
	extractedPath, err := extractRuntime(archivePath, parent, pin)
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(extractedPath) }()
	if err := commitVerifiedTemp(extractedPath, target, pin.SHA256); err != nil {
		return "", fmt.Errorf("caching ONNX Runtime at %s: %w", target, err)
	}
	if err := verifyFile(target, pin.SHA256); err != nil {
		return "", fmt.Errorf("verifying cached ONNX Runtime %s: %w", target, err)
	}
	return target, nil
}

func (p *provisioner) ensureDirectFile(ctx context.Context, target, upstream, expected string) ([]byte, error) {
	data, err := readVerifiedFile(target, expected)
	if err == nil {
		return data, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if p.offline {
		return nil, fmt.Errorf("%s is missing in offline mode; fetch %s and pre-position it with sha256=%s", target, upstream, expected)
	}
	parent := filepath.Dir(target)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return nil, fmt.Errorf("creating model cache directory %s: %w", parent, err)
	}
	tempPath, err := p.fetchVerified(ctx, upstream, parent, ".model-download-*", expected)
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.Remove(tempPath) }()
	if err := commitVerifiedTemp(tempPath, target, expected); err != nil {
		return nil, fmt.Errorf("caching at %s: %w", target, err)
	}
	return readVerifiedFile(target, expected)
}

func (p *provisioner) fetchVerified(ctx context.Context, upstream, dir, pattern, expected string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstream, nil)
	if err != nil {
		return "", fmt.Errorf("creating request for %s: %w", upstream, err)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetching %s: %w", upstream, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("fetching %s: HTTP %s", upstream, resp.Status)
	}
	temp, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", fmt.Errorf("creating temporary artifact in %s: %w", dir, err)
	}
	tempPath := temp.Name()
	ok := false
	defer func() {
		_ = temp.Close()
		if !ok {
			_ = os.Remove(tempPath)
		}
	}()

	h := sha256.New()
	written, err := io.Copy(io.MultiWriter(temp, h), io.LimitReader(resp.Body, maxArtifactBytes+1))
	if err != nil {
		return "", fmt.Errorf("downloading %s: %w", upstream, err)
	}
	if written > maxArtifactBytes {
		return "", fmt.Errorf("downloading %s: artifact exceeds %d bytes", upstream, maxArtifactBytes)
	}
	actual := hex.EncodeToString(h.Sum(nil))
	if actual != expected {
		return "", fmt.Errorf("downloaded %s: sha256 mismatch: expected %s, got %s", upstream, expected, actual)
	}
	if err := temp.Sync(); err != nil {
		return "", fmt.Errorf("syncing downloaded %s: %w", upstream, err)
	}
	if err := temp.Close(); err != nil {
		return "", fmt.Errorf("closing downloaded %s: %w", upstream, err)
	}
	ok = true
	return tempPath, nil
}

func extractRuntime(archivePath, dir string, pin runtimePin) (string, error) {
	temp, err := os.CreateTemp(dir, ".runtime-library-*")
	if err != nil {
		return "", fmt.Errorf("creating temporary runtime library in %s: %w", dir, err)
	}
	tempPath := temp.Name()
	ok := false
	defer func() {
		_ = temp.Close()
		if !ok {
			_ = os.Remove(tempPath)
		}
	}()

	written := false
	writeMember := func(r io.Reader) error {
		if written {
			return fmt.Errorf("archive contains duplicate member %s", pin.ArchiveMember)
		}
		written = true
		h := sha256.New()
		n, err := io.Copy(io.MultiWriter(temp, h), io.LimitReader(r, maxArtifactBytes+1))
		if err != nil {
			return err
		}
		if n > maxArtifactBytes {
			return fmt.Errorf("runtime library exceeds %d bytes", maxArtifactBytes)
		}
		actual := hex.EncodeToString(h.Sum(nil))
		if actual != pin.SHA256 {
			return fmt.Errorf("extracted runtime %s: sha256 mismatch: expected %s, got %s", pin.ArchiveMember, pin.SHA256, actual)
		}
		return nil
	}

	switch pin.ArchiveFormat {
	case "zip":
		archive, err := zip.OpenReader(archivePath)
		if err != nil {
			return "", fmt.Errorf("opening verified runtime ZIP: %w", err)
		}
		defer func() { _ = archive.Close() }()
		for _, file := range archive.File {
			if normalizeArchivePath(file.Name) != normalizeArchivePath(pin.ArchiveMember) {
				continue
			}
			if !file.Mode().IsRegular() {
				return "", fmt.Errorf("runtime archive member %s is not a regular file", pin.ArchiveMember)
			}
			r, err := file.Open()
			if err != nil {
				return "", fmt.Errorf("opening runtime archive member %s: %w", pin.ArchiveMember, err)
			}
			err = writeMember(r)
			_ = r.Close()
			if err != nil {
				return "", err
			}
		}
	case "tar.gz":
		file, err := os.Open(archivePath)
		if err != nil {
			return "", fmt.Errorf("opening verified runtime tarball: %w", err)
		}
		defer func() { _ = file.Close() }()
		gz, err := gzip.NewReader(file)
		if err != nil {
			return "", fmt.Errorf("opening verified runtime gzip stream: %w", err)
		}
		defer func() { _ = gz.Close() }()
		tr := tar.NewReader(gz)
		for {
			header, err := tr.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return "", fmt.Errorf("reading verified runtime tarball: %w", err)
			}
			if normalizeArchivePath(header.Name) != normalizeArchivePath(pin.ArchiveMember) {
				continue
			}
			if header.Typeflag != tar.TypeReg {
				return "", fmt.Errorf("runtime archive member %s is not a regular file", pin.ArchiveMember)
			}
			if err := writeMember(tr); err != nil {
				return "", err
			}
		}
	default:
		return "", fmt.Errorf("unsupported runtime archive format %q", pin.ArchiveFormat)
	}
	if !written {
		return "", fmt.Errorf("verified runtime archive has no member %s", pin.ArchiveMember)
	}
	if err := temp.Sync(); err != nil {
		return "", fmt.Errorf("syncing extracted runtime: %w", err)
	}
	if err := temp.Close(); err != nil {
		return "", fmt.Errorf("closing extracted runtime: %w", err)
	}
	ok = true
	return tempPath, nil
}

func normalizeArchivePath(path string) string {
	return strings.TrimPrefix(filepath.ToSlash(path), "./")
}

func readVerifiedFile(path, expected string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if info.Size() > maxArtifactBytes {
		return nil, fmt.Errorf("%s exceeds %d bytes", path, maxArtifactBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	actual := fmt.Sprintf("%x", sha256.Sum256(data))
	if actual != expected {
		return nil, fmt.Errorf("%s: sha256 mismatch: expected %s, got %s", path, expected, actual)
	}
	return data, nil
}

func verifyFile(path, expected string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stating %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	if info.Size() > maxArtifactBytes {
		return fmt.Errorf("%s exceeds %d bytes", path, maxArtifactBytes)
	}
	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		return fmt.Errorf("hashing %s: %w", path, err)
	}
	actual := hex.EncodeToString(h.Sum(nil))
	if actual != expected {
		return fmt.Errorf("sha256 mismatch: expected %s, got %s", expected, actual)
	}
	return nil
}

func safeCachePath(root, relative string) (string, error) {
	if !safeRelativePath(filepath.ToSlash(relative)) {
		return "", fmt.Errorf("cache path %q is not relative", relative)
	}
	root, err := canonicalPath(root)
	if err != nil {
		return "", fmt.Errorf("resolving cache root %s: %w", root, err)
	}
	target, err := canonicalPath(filepath.Join(root, relative))
	if err != nil {
		return "", fmt.Errorf("resolving cache path %s: %w", relative, err)
	}
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("cache path %q escapes %s", relative, root)
	}
	return target, nil
}

// canonicalPath resolves every existing symlink component and retains any
// not-yet-created suffix. This keeps cache reads and writes under the intended
// root even when an existing child path is a symlink.
func canonicalPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	current := abs
	var missing []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

// commitVerifiedTemp installs a verified temporary file without replacing an
// existing path. If another process won the race, its file must verify too.
func commitVerifiedTemp(tempPath, target, expected string) error {
	if err := os.Link(tempPath, target); err == nil {
		return nil
	} else if verifyErr := verifyFile(target, expected); verifyErr == nil {
		return nil
	} else {
		return fmt.Errorf("creating cache entry: %w (existing target verification: %v)", err, verifyErr)
	}
}
