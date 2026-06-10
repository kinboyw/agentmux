package updater

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveReleaseFindsRoleAssetAndChecksum(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/owner/repo/releases/latest":
			fmt.Fprintf(w, `{"tag_name":"v1.2.3","assets":[{"name":"agentmux-control-linux-amd64.tar.gz","browser_download_url":"%s/a.tgz"},{"name":"agentmux-control-linux-amd64.tar.gz.sha256","browser_download_url":"%s/a.tgz.sha256"}]}`, serverURL(r), serverURL(r))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	release, err := ResolveRelease(context.Background(), Options{
		Repo: "owner/repo", Role: "control", OS: "linux", Arch: "amd64", BaseAPIURL: server.URL, Client: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if release.Version != "v1.2.3" || release.AssetName != "agentmux-control-linux-amd64.tar.gz" || release.ChecksumURL == "" {
		t.Fatalf("unexpected release: %+v", release)
	}
}

func TestInstallVerifiesChecksumAndExtractsExpectedBinary(t *testing.T) {
	archive := testArchive(t, "agentmux-control-linux-amd64", "new binary")
	sum := sha256.Sum256(archive)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/owner/repo/releases/tags/v1.2.3":
			fmt.Fprintf(w, `{"tag_name":"v1.2.3","assets":[{"name":"agentmux-control-linux-amd64.tar.gz","browser_download_url":"%s/archive"},{"name":"agentmux-control-linux-amd64.tar.gz.sha256","browser_download_url":"%s/archive.sha256"}]}`, serverURL(r), serverURL(r))
		case "/archive":
			_, _ = w.Write(archive)
		case "/archive.sha256":
			fmt.Fprintf(w, "%s  agentmux-control-linux-amd64.tar.gz\n", hex.EncodeToString(sum[:]))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	installPath := filepath.Join(t.TempDir(), "agentmux")
	if err := os.WriteFile(installPath, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	result, err := Install(context.Background(), installPath, Options{
		Repo: "owner/repo", Version: "v1.2.3", Role: "control", OS: "linux", Arch: "amd64", BaseAPIURL: server.URL, Client: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(result.Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new binary" {
		t.Fatalf("unexpected binary content: %q", data)
	}
	if result.PreviousPath == "" {
		t.Fatal("expected previous path")
	}
}

func TestResolveReleaseFallsBackToLegacyWorkerAsset(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"tag_name":"v0.0.1","assets":[{"name":"agentmux-linux-amd64.tar.gz","browser_download_url":"%s/legacy"},{"name":"agentmux-linux-amd64.tar.gz.sha256","browser_download_url":"%s/legacy.sha256"}]}`, serverURL(r), serverURL(r))
	}))
	defer server.Close()

	release, err := ResolveRelease(context.Background(), Options{
		Repo: "owner/repo", Role: "worker", OS: "linux", Arch: "amd64", BaseAPIURL: server.URL, Client: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if release.AssetName != "agentmux-linux-amd64.tar.gz" || release.ExpectedEntry != "agentmux-linux-amd64" {
		t.Fatalf("unexpected fallback release: %+v", release)
	}
}

func TestPruneCacheRemovesReleaseCache(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("AGENTMUX_CACHE_DIR", cache)
	releaseFile := filepath.Join(cache, "releases", "v1.0.0", "control", "linux-amd64", "agentmux")
	if err := os.MkdirAll(filepath.Dir(releaseFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(releaseFile, []byte("cached"), 0o755); err != nil {
		t.Fatal(err)
	}
	metadataFile := filepath.Join(cache, "metadata.json")
	if err := os.WriteFile(metadataFile, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := PruneCache()
	if err != nil {
		t.Fatal(err)
	}
	if !result.Removed || result.BytesRemoved == 0 {
		t.Fatalf("unexpected prune result: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(cache, "releases")); !os.IsNotExist(err) {
		t.Fatalf("expected releases cache removed, err=%v", err)
	}
	if _, err := os.Stat(metadataFile); err != nil {
		t.Fatalf("metadata should remain: %v", err)
	}
}

func testArchive(t *testing.T, name, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	data := []byte(content)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(data))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func serverURL(r *http.Request) string {
	return "http://" + r.Host
}
