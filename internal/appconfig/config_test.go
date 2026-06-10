package appconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveWorkerBackend(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := SaveWorkerBackend("pty"); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WorkerBackend != "pty" {
		t.Fatalf("unexpected backend: %q", cfg.WorkerBackend)
	}
}

func TestSaveWorkerAuth(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := SaveWorkerAuth("wss://hub.example/ws", "amx_cred_test", "worker-1", "Worker One"); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WorkerHubURL != "wss://hub.example/ws" || cfg.WorkerToken != "" || cfg.WorkerID != "worker-1" || cfg.WorkerName != "Worker One" {
		t.Fatalf("unexpected worker auth config: %+v", cfg)
	}
	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "worker_token") || strings.Contains(string(raw), "amx_cred_test") {
		t.Fatalf("config should not persist credentials: %s", raw)
	}
}

func TestLoadKeepsLegacyWorkerToken(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"worker_hub_url":"wss://hub.example/ws","worker_token":"amx_legacy","worker_id":"worker-1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WorkerHubURL != "wss://hub.example/ws" || cfg.WorkerToken != "amx_legacy" || cfg.WorkerID != "worker-1" {
		t.Fatalf("unexpected legacy worker config: %+v", cfg)
	}
}
