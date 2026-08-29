package embedding

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kninetimmy/memdolt/models"
	ort "github.com/yalue/onnxruntime_go"
)

func TestDefaultManifestSupportsEveryClientPlatform(t *testing.T) {
	m, err := loadManifest(models.ManifestJSON())
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	want := map[string]bool{
		"windows/amd64": false,
		"linux/amd64":   false,
		"linux/arm64":   false,
		"darwin/arm64":  false,
	}
	for _, pin := range m.Runtimes {
		platform := pin.GOOS + "/" + pin.GOARCH
		if _, ok := want[platform]; !ok {
			t.Errorf("unexpected runtime platform %s", platform)
			continue
		}
		want[platform] = true
	}
	for platform, found := range want {
		if !found {
			t.Errorf("manifest has no runtime for %s", platform)
		}
	}
	for _, bundle := range m.Bundles {
		if strings.Contains(bundle.BaseURL, "/resolve/main") {
			t.Errorf("bundle %s uses mutable main URL: %s", bundle.Name, bundle.BaseURL)
		}
	}
}

func TestProvisionOfflineUsesOnlyVerifiedPrepositionedFiles(t *testing.T) {
	modelData := []byte("verified model")
	runtimeData := []byte("verified runtime")
	m := testManifest("https://example.invalid", "zip", modelData, runtimeData, nil)
	cache := t.TempDir()
	writeTestFile(t, filepath.Join(cache, "test-model", "model.onnx"), modelData)
	writeTestFile(t, filepath.Join(cache, filepath.FromSlash(m.Runtimes[0].LocalPath)), runtimeData)

	p := provisioner{manifest: m, cacheDir: cache, offline: true, goos: "testos", goarch: "testarch"}
	got, err := p.provision(context.Background())
	if err != nil {
		t.Fatalf("provision offline: %v", err)
	}
	if string(got.models["test-model"]["model.onnx"]) != string(modelData) {
		t.Fatal("provisioned model bytes differ")
	}
	if err := verifyFile(got.runtimePath, sha(runtimeData)); err != nil {
		t.Fatalf("verify returned runtime: %v", err)
	}
}

func TestProvisionFetchesAndVerifiesMissingArtifacts(t *testing.T) {
	for _, format := range []string{"zip", "tar.gz"} {
		t.Run(format, func(t *testing.T) {
			modelData := []byte("downloaded model")
			runtimeData := []byte("downloaded runtime")
			member := "runtime/lib/runtime.bin"
			archiveData := testArchive(t, format, member, runtimeData)
			var requests atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				switch r.URL.Path {
				case "/models/model.onnx":
					_, _ = w.Write(modelData)
				case "/runtime":
					_, _ = w.Write(archiveData)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			m := testManifest(server.URL+"/models", format, modelData, runtimeData, archiveData)
			m.Runtimes[0].URL = server.URL + "/runtime"
			m.Runtimes[0].ArchiveMember = member
			p := provisioner{
				manifest: m, cacheDir: t.TempDir(), client: server.Client(),
				goos: "testos", goarch: "testarch",
			}
			got, err := p.provision(context.Background())
			if err != nil {
				t.Fatalf("provision: %v", err)
			}
			if requests.Load() != 2 {
				t.Fatalf("requests = %d, want 2", requests.Load())
			}
			if string(got.models["test-model"]["model.onnx"]) != string(modelData) {
				t.Fatal("downloaded model bytes differ")
			}
			if err := verifyFile(got.runtimePath, sha(runtimeData)); err != nil {
				t.Fatalf("verify downloaded runtime: %v", err)
			}
		})
	}
}

func TestProvisionRejectsChecksumMismatchesWithoutTrustingThem(t *testing.T) {
	t.Run("existing cache entry", func(t *testing.T) {
		modelData := []byte("expected model")
		runtimeData := []byte("runtime")
		m := testManifest("https://example.invalid", "zip", modelData, runtimeData, nil)
		cache := t.TempDir()
		modelPath := filepath.Join(cache, "test-model", "model.onnx")
		writeTestFile(t, modelPath, []byte("tampered"))
		writeTestFile(t, filepath.Join(cache, filepath.FromSlash(m.Runtimes[0].LocalPath)), runtimeData)

		p := provisioner{manifest: m, cacheDir: cache, offline: true, goos: "testos", goarch: "testarch"}
		_, err := p.provision(context.Background())
		if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
			t.Fatalf("error = %v, want checksum mismatch", err)
		}
		if ort.IsInitialized() {
			t.Fatal("ONNX Runtime initialized despite an artifact checksum mismatch")
		}
		got, readErr := os.ReadFile(modelPath)
		if readErr != nil || string(got) != "tampered" {
			t.Fatalf("mismatched operator file was modified: data=%q err=%v", got, readErr)
		}
	})

	t.Run("downloaded model", func(t *testing.T) {
		expectedModel := []byte("expected model")
		runtimeData := []byte("runtime")
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "tampered download")
		}))
		defer server.Close()
		m := testManifest(server.URL, "zip", expectedModel, runtimeData, nil)
		cache := t.TempDir()
		writeTestFile(t, filepath.Join(cache, filepath.FromSlash(m.Runtimes[0].LocalPath)), runtimeData)
		p := provisioner{
			manifest: m, cacheDir: cache, client: server.Client(),
			goos: "testos", goarch: "testarch",
		}
		_, err := p.provision(context.Background())
		if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
			t.Fatalf("error = %v, want checksum mismatch", err)
		}
		modelPath := filepath.Join(cache, "test-model", "model.onnx")
		if _, statErr := os.Stat(modelPath); !os.IsNotExist(statErr) {
			t.Fatalf("mismatched download became a cache entry: %v", statErr)
		}
		if matches, _ := filepath.Glob(filepath.Join(filepath.Dir(modelPath), ".model-download-*")); len(matches) != 0 {
			t.Fatalf("mismatched download left temporary files: %v", matches)
		}
	})

	for _, tc := range []struct {
		name          string
		archiveSHA256 string
		runtimeSHA256 string
		want          string
	}{
		{name: "runtime archive", archiveSHA256: sha([]byte("different archive")), runtimeSHA256: sha([]byte("runtime")), want: "sha256 mismatch"},
		{name: "extracted runtime", archiveSHA256: "", runtimeSHA256: sha([]byte("different runtime")), want: "extracted runtime"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtimeData := []byte("runtime")
			archiveData := testArchive(t, "zip", "runtime/lib/runtime.bin", runtimeData)
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write(archiveData)
			}))
			defer server.Close()
			m := testManifest("https://example.invalid", "zip", []byte("model"), runtimeData, archiveData)
			m.Runtimes[0].URL = server.URL
			m.Runtimes[0].ArchiveMember = "runtime/lib/runtime.bin"
			if tc.archiveSHA256 != "" {
				m.Runtimes[0].ArchiveSHA256 = tc.archiveSHA256
			}
			m.Runtimes[0].SHA256 = tc.runtimeSHA256
			cache := t.TempDir()
			p := provisioner{
				manifest: m, cacheDir: cache, client: server.Client(),
				goos: "testos", goarch: "testarch",
			}
			_, err := p.provision(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			target := filepath.Join(cache, filepath.FromSlash(m.Runtimes[0].LocalPath))
			if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
				t.Fatalf("mismatched runtime became a cache entry: %v", statErr)
			}
		})
	}
}

func TestSafeCachePathRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "bundle")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := safeCachePath(root, filepath.Join("bundle", "model.onnx")); err == nil {
		t.Fatal("safeCachePath accepted a child symlink outside the cache root")
	}
}

func testManifest(baseURL, format string, modelData, runtimeData, archiveData []byte) artifactManifest {
	archiveSHA := sha(archiveData)
	if archiveData == nil {
		archiveSHA = sha([]byte("unused archive"))
	}
	return artifactManifest{
		Version: 1, ONNXRuntimeVersion: "test",
		Bundles: []modelBundlePin{{
			Name: "test-model", Revision: "immutable", BaseURL: baseURL,
			Files: []modelFilePin{{RemotePath: "model.onnx", LocalName: "model.onnx", SHA256: sha(modelData)}},
		}},
		Runtimes: []runtimePin{{
			GOOS: "testos", GOARCH: "testarch", LocalPath: "runtime/test/runtime.bin",
			SHA256: sha(runtimeData), URL: baseURL + "/runtime", ArchiveFormat: format,
			ArchiveMember: "runtime/lib/runtime.bin", ArchiveSHA256: archiveSHA,
		}},
	}
}

func testArchive(t *testing.T, format, member string, data []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	switch format {
	case "zip":
		zw := zip.NewWriter(&out)
		w, err := zw.Create(member)
		if err != nil {
			t.Fatalf("create zip member: %v", err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatalf("write zip member: %v", err)
		}
		if err := zw.Close(); err != nil {
			t.Fatalf("close zip: %v", err)
		}
	case "tar.gz":
		gz := gzip.NewWriter(&out)
		tw := tar.NewWriter(gz)
		if err := tw.WriteHeader(&tar.Header{Name: member, Mode: 0o600, Size: int64(len(data))}); err != nil {
			t.Fatalf("write tar header: %v", err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatalf("write tar member: %v", err)
		}
		if err := tw.Close(); err != nil {
			t.Fatalf("close tar: %v", err)
		}
		if err := gz.Close(); err != nil {
			t.Fatalf("close gzip: %v", err)
		}
	default:
		t.Fatalf("unsupported test archive format %q", format)
	}
	return out.Bytes()
}

func writeTestFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func sha(data []byte) string {
	return fmt.Sprintf("%x", sha256.Sum256(data))
}
