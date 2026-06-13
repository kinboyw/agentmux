package updater

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const defaultRepo = "kinboyw/agentmux"

type Options struct {
	Repo       string
	Version    string
	Role       string
	OS         string
	Arch       string
	BaseAPIURL string
	Client     *http.Client
}

type Release struct {
	Repo          string
	Version       string
	Role          string
	OS            string
	Arch          string
	AssetName     string
	AssetURL      string
	ChecksumName  string
	ChecksumURL   string
	ExpectedEntry string
}

type CheckResult struct {
	Current         string `json:"current"`
	Latest          string `json:"latest"`
	UpdateAvailable bool   `json:"update_available"`
	Role            string `json:"role"`
	OS              string `json:"os"`
	Arch            string `json:"arch"`
	AssetName       string `json:"asset_name"`
	AssetURL        string `json:"asset_url"`
	ChecksumURL     string `json:"checksum_url,omitempty"`
}

type InstallResult struct {
	Path         string `json:"path"`
	PreviousPath string `json:"previous_path,omitempty"`
	Version      string `json:"version"`
	Role         string `json:"role"`
	AssetName    string `json:"asset_name"`
}

type RunResult struct {
	Path    string
	Version string
	Role    string
}

type PruneResult struct {
	Path         string `json:"path"`
	Removed      bool   `json:"removed"`
	BytesRemoved int64  `json:"bytes_removed"`
}

type rollbackManifest struct {
	Path         string    `json:"path"`
	PreviousPath string    `json:"previous_path"`
	Version      string    `json:"version"`
	Role         string    `json:"role"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type githubRelease struct {
	TagName string        `json:"tag_name"`
	Assets  []githubAsset `json:"assets"`
}

type githubAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

func NormalizeOptions(options Options) (Options, error) {
	options.Repo = strings.TrimSpace(options.Repo)
	if options.Repo == "" {
		options.Repo = defaultRepo
	}
	if !strings.Contains(options.Repo, "/") {
		return options, fmt.Errorf("release repo must be owner/repo, got %q", options.Repo)
	}
	options.Version = strings.TrimSpace(options.Version)
	if options.Version == "" {
		options.Version = "latest"
	}
	options.Role = strings.ToLower(strings.TrimSpace(options.Role))
	if options.Role == "" {
		options.Role = "control"
	}
	switch options.Role {
	case "worker", "control", "hub":
	default:
		return options, fmt.Errorf("invalid role %q; expected worker, control, or hub", options.Role)
	}
	if options.OS == "" {
		options.OS = runtime.GOOS
	}
	if options.Arch == "" {
		options.Arch = runtime.GOARCH
	}
	if options.BaseAPIURL == "" {
		options.BaseAPIURL = "https://api.github.com"
	}
	if options.Client == nil {
		options.Client = http.DefaultClient
	}
	return options, nil
}

func ResolveRelease(ctx context.Context, options Options) (Release, error) {
	options, err := NormalizeOptions(options)
	if err != nil {
		return Release{}, err
	}
	endpoint := strings.TrimRight(options.BaseAPIURL, "/") + "/repos/" + options.Repo + "/releases/latest"
	if options.Version != "latest" {
		endpoint = strings.TrimRight(options.BaseAPIURL, "/") + "/repos/" + options.Repo + "/releases/tags/" + options.Version
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := options.Client.Do(req)
	if err != nil {
		return Release{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return Release{}, fmt.Errorf("resolve release failed: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var release githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return Release{}, err
	}
	tag := strings.TrimSpace(release.TagName)
	if tag == "" {
		tag = options.Version
	}
	assetBases := []string{fmt.Sprintf("agentmux-%s-%s-%s", options.Role, options.OS, options.Arch)}
	if options.Role == "control" {
		assetBases = []string{
			fmt.Sprintf("agentmux-tui-%s-%s", options.OS, options.Arch),
			fmt.Sprintf("agentmux-control-%s-%s", options.OS, options.Arch),
		}
	}
	if options.Role != "hub" {
		assetBases = append(assetBases, fmt.Sprintf("agentmux-%s-%s", options.OS, options.Arch))
	}
	var assetName, entryName, assetURL string
	for _, candidate := range assetBases {
		candidateName := candidate + ".tar.gz"
		if url := findAsset(release.Assets, candidateName); url != "" {
			assetName = candidateName
			entryName = candidate
			assetURL = url
			break
		}
	}
	if options.OS == "windows" && options.Role == "hub" {
		entryName += ".exe"
	}
	if assetURL == "" {
		return Release{}, fmt.Errorf("release %s has no asset %s", tag, assetBases[0]+".tar.gz")
	}
	checksumName := assetName + ".sha256"
	return Release{
		Repo: options.Repo, Version: tag, Role: options.Role, OS: options.OS, Arch: options.Arch,
		AssetName: assetName, AssetURL: assetURL, ChecksumName: checksumName,
		ChecksumURL: findAsset(release.Assets, checksumName), ExpectedEntry: entryName,
	}, nil
}

func Check(ctx context.Context, current string, options Options) (CheckResult, error) {
	release, err := ResolveRelease(ctx, options)
	if err != nil {
		return CheckResult{}, err
	}
	return CheckResult{
		Current: strings.TrimSpace(current), Latest: release.Version,
		UpdateAvailable: updateAvailable(current, release.Version),
		Role:            release.Role, OS: release.OS, Arch: release.Arch,
		AssetName: release.AssetName, AssetURL: release.AssetURL, ChecksumURL: release.ChecksumURL,
	}, nil
}

func DownloadToCache(ctx context.Context, options Options) (RunResult, error) {
	options, err := NormalizeOptions(options)
	if err != nil {
		return RunResult{}, err
	}
	release, err := ResolveRelease(ctx, options)
	if err != nil {
		return RunResult{}, err
	}
	root, err := cacheDir()
	if err != nil {
		return RunResult{}, err
	}
	targetDir := filepath.Join(root, "releases", safePathPart(release.Version), release.Role, release.OS+"-"+release.Arch)
	targetPath := filepath.Join(targetDir, runtimeBinaryName(release.Role))
	if fileExecutable(targetPath) {
		return RunResult{Path: targetPath, Version: release.Version, Role: release.Role}, nil
	}
	tmpDir, err := os.MkdirTemp(root, "download-*")
	if err != nil {
		return RunResult{}, err
	}
	defer os.RemoveAll(tmpDir)
	extracted, err := downloadAndExtract(ctx, release, tmpDir, options.Client)
	if err != nil {
		return RunResult{}, err
	}
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return RunResult{}, err
	}
	if err := copyExecutable(extracted, targetPath); err != nil {
		return RunResult{}, err
	}
	return RunResult{Path: targetPath, Version: release.Version, Role: release.Role}, nil
}

func Install(ctx context.Context, installPath string, options Options) (InstallResult, error) {
	if runtime.GOOS == "windows" {
		return InstallResult{}, errors.New("in-place apply is not supported on Windows yet; download the new hub asset and replace the executable after stopping the service")
	}
	options, err := NormalizeOptions(options)
	if err != nil {
		return InstallResult{}, err
	}
	installPath = strings.TrimSpace(installPath)
	if installPath == "" {
		return InstallResult{}, errors.New("install path is required")
	}
	release, err := ResolveRelease(ctx, options)
	if err != nil {
		return InstallResult{}, err
	}
	tmpDir, err := os.MkdirTemp("", "agentmux-update-*")
	if err != nil {
		return InstallResult{}, err
	}
	defer os.RemoveAll(tmpDir)
	extracted, err := downloadAndExtract(ctx, release, tmpDir, options.Client)
	if err != nil {
		return InstallResult{}, err
	}
	absInstallPath, err := filepath.Abs(installPath)
	if err != nil {
		return InstallResult{}, err
	}
	parent := filepath.Dir(absInstallPath)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return InstallResult{}, err
	}
	staged := filepath.Join(parent, fmt.Sprintf(".%s.update.%d", filepath.Base(absInstallPath), time.Now().UnixNano()))
	if err := copyExecutable(extracted, staged); err != nil {
		return InstallResult{}, err
	}
	previous := filepath.Join(parent, fmt.Sprintf(".%s.previous.%d", filepath.Base(absInstallPath), time.Now().UnixNano()))
	if _, err := os.Stat(absInstallPath); err == nil {
		if err := os.Rename(absInstallPath, previous); err != nil {
			_ = os.Remove(staged)
			return InstallResult{}, fmt.Errorf("stage previous binary: %w", err)
		}
	} else if !os.IsNotExist(err) {
		_ = os.Remove(staged)
		return InstallResult{}, err
	} else {
		previous = ""
	}
	if err := os.Rename(staged, absInstallPath); err != nil {
		if previous != "" {
			_ = os.Rename(previous, absInstallPath)
		}
		_ = os.Remove(staged)
		return InstallResult{}, fmt.Errorf("install staged binary: %w", err)
	}
	if previous != "" {
		_ = saveRollback(rollbackManifest{Path: absInstallPath, PreviousPath: previous, Version: release.Version, Role: release.Role, UpdatedAt: time.Now().UTC()})
	}
	return InstallResult{Path: absInstallPath, PreviousPath: previous, Version: release.Version, Role: release.Role, AssetName: release.AssetName}, nil
}

func Rollback() (InstallResult, error) {
	manifest, err := loadRollback()
	if err != nil {
		return InstallResult{}, err
	}
	if runtime.GOOS == "windows" {
		return InstallResult{}, errors.New("rollback is not supported on Windows yet")
	}
	if manifest.Path == "" || manifest.PreviousPath == "" {
		return InstallResult{}, errors.New("rollback manifest is incomplete")
	}
	if _, err := os.Stat(manifest.PreviousPath); err != nil {
		return InstallResult{}, fmt.Errorf("previous binary is unavailable: %w", err)
	}
	currentBackup := manifest.Path + ".rollback-current." + fmt.Sprint(time.Now().UnixNano())
	if _, err := os.Stat(manifest.Path); err == nil {
		if err := os.Rename(manifest.Path, currentBackup); err != nil {
			return InstallResult{}, err
		}
	}
	if err := os.Rename(manifest.PreviousPath, manifest.Path); err != nil {
		_ = os.Rename(currentBackup, manifest.Path)
		return InstallResult{}, err
	}
	_ = os.Remove(rollbackPath())
	return InstallResult{Path: manifest.Path, PreviousPath: currentBackup, Version: manifest.Version, Role: manifest.Role}, nil
}

func Exec(ctx context.Context, binary string, args []string) error {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func PruneCache() (PruneResult, error) {
	root, err := cacheDir()
	if err != nil {
		return PruneResult{}, err
	}
	releases := filepath.Join(root, "releases")
	size, _ := dirSize(releases)
	if err := os.RemoveAll(releases); err != nil {
		return PruneResult{}, err
	}
	return PruneResult{Path: releases, Removed: true, BytesRemoved: size}, nil
}

func findAsset(assets []githubAsset, name string) string {
	for _, asset := range assets {
		if asset.Name == name {
			return asset.BrowserDownloadURL
		}
	}
	return ""
}

func updateAvailable(current, latest string) bool {
	current = normalizeVersion(current)
	latest = normalizeVersion(latest)
	return current != "" && latest != "" && current != latest
}

func normalizeVersion(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "refs/tags/")
	return value
}

func downloadAndExtract(ctx context.Context, release Release, tmpDir string, client *http.Client) (string, error) {
	if client == nil {
		client = http.DefaultClient
	}
	archivePath := filepath.Join(tmpDir, release.AssetName)
	if err := download(ctx, client, release.AssetURL, archivePath); err != nil {
		return "", err
	}
	if release.ChecksumURL == "" {
		return "", fmt.Errorf("release asset %s has no checksum asset %s", release.AssetName, release.ChecksumName)
	}
	checksumPath := archivePath + ".sha256"
	if err := download(ctx, client, release.ChecksumURL, checksumPath); err != nil {
		return "", err
	}
	expected, err := readChecksum(checksumPath)
	if err != nil {
		return "", err
	}
	if err := verifySHA256(archivePath, expected); err != nil {
		return "", err
	}
	extractDir := filepath.Join(tmpDir, "extract")
	if err := os.MkdirAll(extractDir, 0o700); err != nil {
		return "", err
	}
	return extractTarGz(archivePath, extractDir, release.ExpectedEntry)
}

func download(ctx context.Context, client *http.Client, url, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("download %s failed: %s: %s", url, resp.Status, strings.TrimSpace(string(body)))
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = io.Copy(file, resp.Body)
	return err
}

func readChecksum(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return "", fmt.Errorf("empty checksum file")
	}
	expected := strings.ToLower(strings.TrimSpace(fields[0]))
	if len(expected) != sha256.Size*2 {
		return "", fmt.Errorf("invalid sha256 length")
	}
	if _, err := hex.DecodeString(expected); err != nil {
		return "", err
	}
	return expected, nil
}

func verifySHA256(path, expected string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, file); err != nil {
		return err
	}
	actual := hex.EncodeToString(sum.Sum(nil))
	if actual != expected {
		return fmt.Errorf("sha256 mismatch for %s: got %s want %s", filepath.Base(path), actual, expected)
	}
	return nil
}

func extractTarGz(path, dest, expectedEntry string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		return "", err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var extracted string
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		name := filepath.Clean(header.Name)
		if name == "." || strings.HasPrefix(name, "..") || filepath.IsAbs(name) {
			return "", fmt.Errorf("unsafe archive entry %q", header.Name)
		}
		if name != expectedEntry {
			continue
		}
		if header.Typeflag != tar.TypeReg {
			return "", fmt.Errorf("expected regular file %q", name)
		}
		extracted = filepath.Join(dest, name)
		if err := os.MkdirAll(filepath.Dir(extracted), 0o700); err != nil {
			return "", err
		}
		out, err := os.OpenFile(extracted, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o700)
		if err != nil {
			return "", err
		}
		_, copyErr := io.Copy(out, tr)
		closeErr := out.Close()
		if copyErr != nil {
			return "", copyErr
		}
		if closeErr != nil {
			return "", closeErr
		}
	}
	if extracted == "" {
		return "", fmt.Errorf("archive does not contain %q", expectedEntry)
	}
	return extracted, nil
}

func copyExecutable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Chmod(dst, 0o755)
}

func runtimeBinaryName(role string) string {
	if role == "hub" {
		if runtime.GOOS == "windows" {
			return "agentmux-hub.exe"
		}
		return "agentmux-hub"
	}
	if role == "control" {
		if runtime.GOOS == "windows" {
			return "agentmux-tui.exe"
		}
		return "agentmux-tui"
	}
	if runtime.GOOS == "windows" {
		return "agentmux.exe"
	}
	return "agentmux"
}

func fileExecutable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}

func cacheDir() (string, error) {
	if value := strings.TrimSpace(os.Getenv("AGENTMUX_CACHE_DIR")); value != "" {
		if err := os.MkdirAll(value, 0o755); err != nil {
			return "", err
		}
		return value, nil
	}
	if value := strings.TrimSpace(os.Getenv("XDG_CACHE_HOME")); value != "" {
		path := filepath.Join(value, "agentmux")
		if err := os.MkdirAll(path, 0o755); err != nil {
			return "", err
		}
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(home, ".cache", "agentmux")
	if err := os.MkdirAll(path, 0o755); err != nil {
		return "", err
	}
	return path, nil
}

func stateDir() (string, error) {
	if value := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); value != "" {
		path := filepath.Join(value, "agentmux", "update")
		if err := os.MkdirAll(path, 0o700); err != nil {
			return "", err
		}
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(home, ".local", "state", "agentmux", "update")
	if err := os.MkdirAll(path, 0o700); err != nil {
		return "", err
	}
	return path, nil
}

func rollbackPath() string {
	dir, err := stateDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "rollback.json")
}

func saveRollback(manifest rollbackManifest) error {
	path := rollbackPath()
	if path == "" {
		return nil
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func loadRollback() (rollbackManifest, error) {
	path := rollbackPath()
	if path == "" {
		return rollbackManifest{}, errors.New("update state directory is unavailable")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return rollbackManifest{}, err
	}
	var manifest rollbackManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return rollbackManifest{}, err
	}
	return manifest, nil
}

func safePathPart(value string) string {
	value = strings.TrimSpace(value)
	value = strings.ReplaceAll(value, "/", "_")
	value = strings.ReplaceAll(value, "\\", "_")
	value = strings.ReplaceAll(value, "..", "_")
	if value == "" {
		return "unknown"
	}
	return value
}

func dirSize(path string) (int64, error) {
	var total int64
	err := filepath.WalkDir(path, func(_ string, entry os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}
