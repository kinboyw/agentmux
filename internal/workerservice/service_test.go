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
