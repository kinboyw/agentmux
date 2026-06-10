package main

import (
	"testing"
	"time"

	"private/agentmux/internal/appconfig"
	"private/agentmux/internal/credentialcache"
)

func TestWorkerCredentialFromCacheRequiresConfiguredHub(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := credentialcache.Save(credentialcache.Entry{
		HubURL: "https://other.test", Role: "worker", DeviceID: "worker-1",
		Credential: "amx_cred_other", ExpiresAt: time.Now().UTC().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := workerCredentialFromCache("https://configured.test", "worker-1"); ok {
		t.Fatal("configured hub should not fall back to another hub credential")
	}
	entry, ok := workerCredentialFromCache("", "worker-1")
	if !ok {
		t.Fatal("empty configured hub should use latest matching worker credential")
	}
	if entry.HubURL != "https://other.test" || entry.Credential != "amx_cred_other" {
		t.Fatalf("unexpected credential: %+v", entry)
	}
}

func TestMigrateLegacyWorkerCredential(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg := appconfig.Config{
		WorkerHubURL: "wss://legacy.test/ws/worker",
		WorkerToken:  "amx_legacy",
		WorkerID:     "worker-1",
		WorkerName:   "Legacy Worker",
	}
	if err := migrateLegacyWorkerCredential(cfg); err != nil {
		t.Fatal(err)
	}
	entry, ok := credentialcache.Load("https://legacy.test", "worker", "worker-1")
	if !ok {
		t.Fatal("expected migrated worker credential")
	}
	if entry.Credential != "amx_legacy" || entry.DeviceName != "Legacy Worker" {
		t.Fatalf("unexpected migrated credential: %+v", entry)
	}
}
