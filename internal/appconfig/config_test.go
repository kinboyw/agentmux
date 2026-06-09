package appconfig

import "testing"

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
