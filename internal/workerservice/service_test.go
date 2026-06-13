package workerservice

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServiceUnavailableNoteCompactsSystemdBusError(t *testing.T) {
	note := serviceUnavailableNote("systemd --user", errors.New("systemctl --user show --property=Version --value: exit status 1: Failed to connect to bus: No such file or directory"))
	if !strings.Contains(note, "using fallback process") {
		t.Fatalf("expected fallback note, got %q", note)
	}
	if strings.Contains(note, "daemon-reload failed") || strings.Contains(note, "exit status") {
		t.Fatalf("expected compact non-alarming note, got %q", note)
	}
	if !strings.Contains(note, "systemd user bus is not available") {
		t.Fatalf("expected compact systemd reason, got %q", note)
	}
}

func TestWorkerLockPathSanitizesID(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)

	path, err := WorkerLockPath(`my/worker:id name`)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(stateHome, "agentmux", "worker-my_worker_id_name.lock")
	if path != want {
		t.Fatalf("unexpected lock path:\n got: %s\nwant: %s", path, want)
	}
}

func TestLockOwnerPIDReadsWrittenOwner(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	path, err := WorkerLockPath("local")
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	WriteLockOwner(file, os.Getpid())
	pid, gotPath, ok := LockOwnerPID("local")
	if !ok || pid != os.Getpid() || gotPath != path {
		t.Fatalf("unexpected lock owner: pid=%d path=%s ok=%v", pid, gotPath, ok)
	}

	ClearLockOwner(file)
	if pid, _, ok := LockOwnerPID("local"); ok {
		t.Fatalf("expected cleared owner, got pid=%d", pid)
	}
}

func TestPrepareWorkerBinaryUsesUniqueCopy(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	src := filepath.Join(t.TempDir(), "agentmux")
	if err := os.WriteFile(src, []byte("test-binary"), 0o700); err != nil {
		t.Fatal(err)
	}

	first, err := prepareWorkerBinary(src)
	if err != nil {
		t.Fatal(err)
	}
	second, err := prepareWorkerBinary(src)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("expected unique worker binary copies, got %s", first)
	}
	for _, path := range []string{first, second} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != "test-binary" {
			t.Fatalf("unexpected copied binary content in %s: %q", path, string(data))
		}
	}
}

func TestWorkerServicePathIncludesHomebrewAndDeduplicates(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/opt/homebrew/bin:/usr/bin")
	got := workerServicePath()
	if !strings.Contains(got, "/opt/homebrew/bin") || !strings.Contains(got, "/usr/local/bin") {
		t.Fatalf("expected macOS package manager paths, got %q", got)
	}
	if strings.Count(got, "/usr/bin") != 1 {
		t.Fatalf("expected deduplicated PATH, got %q", got)
	}
}

func TestLaunchdEnvXMLIncludesConfiguredTmux(t *testing.T) {
	got := launchdEnvXML(map[string]string{
		"PATH":                            "/opt/homebrew/bin:/usr/bin",
		"AGENTMUX_WORKER_INSTALL_KIND":    "service",
		"AGENTMUX_WORKER_SERVICE_BACKEND": "launchd",
		"AGENTMUX_TMUX":                   "/custom/tmux",
	})
	for _, want := range []string{
		"<key>PATH</key><string>/opt/homebrew/bin:/usr/bin</string>",
		"<key>AGENTMUX_TMUX</key><string>/custom/tmux</string>",
		"<key>AGENTMUX_WORKER_SERVICE_BACKEND</key><string>launchd</string>",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("launchd env missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "AGENTMUX_TMUX_PATH") {
		t.Fatalf("empty env should be omitted:\n%s", got)
	}
}
