package credentialcache

import (
	"os"
	"testing"
	"time"
)

func TestEntryRoundTrip(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	entry := Entry{
		HubURL: "wss://hub.test/ws/worker?token=old", Credential: "amx_cred_test", CredentialID: "cred_test",
		TenantID: "tenant_test", Role: "worker", DeviceID: "dev_test",
		DeviceName: "laptop", ExpiresAt: time.Now().UTC().Add(time.Hour), UpdatedAt: time.Now().UTC(),
	}
	if err := Save(entry); err != nil {
		t.Fatal(err)
	}
	got, ok := Load("https://hub.test", "worker", "dev_test")
	if !ok {
		t.Fatal("expected cached credential")
	}
	if got.Credential != entry.Credential || got.DeviceID != entry.DeviceID {
		t.Fatalf("unexpected cache entry: %+v", got)
	}
	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("unexpected cache mode: %o", info.Mode().Perm())
	}
}

func TestLoadIgnoresExpiredEntry(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	entry := Entry{
		HubURL: "http://hub.test", Credential: "amx_cred_test",
		Role: "control", DeviceID: "dev_test", ExpiresAt: time.Now().UTC().Add(-time.Minute),
		UpdatedAt: time.Now().UTC(),
	}
	if err := Save(entry); err != nil {
		t.Fatal(err)
	}
	if _, ok := Load("http://hub.test", "control", "dev_test"); ok {
		t.Fatal("expired credential should not load")
	}
}

func TestSaveReplacesSameHubRoleDevice(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	expires := time.Now().UTC().Add(time.Hour)
	if err := Save(Entry{HubURL: "https://hub.test", Role: "worker", DeviceID: "dev", Credential: "old", ExpiresAt: expires}); err != nil {
		t.Fatal(err)
	}
	if err := Save(Entry{HubURL: "wss://hub.test", Role: "worker", DeviceID: "dev", Credential: "new", ExpiresAt: expires}); err != nil {
		t.Fatal(err)
	}
	got, ok := Load("https://hub.test", "worker", "dev")
	if !ok {
		t.Fatal("expected cached credential")
	}
	if got.Credential != "new" {
		t.Fatalf("expected replacement credential, got %q", got.Credential)
	}
}

func TestLoadLatestByRole(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	expires := time.Now().UTC().Add(time.Hour)
	if err := Save(Entry{
		HubURL: "https://old.test", Role: "worker", DeviceID: "dev",
		Credential: "old", ExpiresAt: expires, UpdatedAt: time.Now().UTC().Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if err := Save(Entry{
		HubURL: "https://new.test", Role: "worker", DeviceID: "dev",
		Credential: "new", ExpiresAt: expires, UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	got, ok := LoadLatest("worker", "dev")
	if !ok {
		t.Fatal("expected latest worker credential")
	}
	if got.HubURL != "https://new.test" || got.Credential != "new" {
		t.Fatalf("unexpected latest credential: %+v", got)
	}
}

func TestLoadLatestAnyRole(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	expires := time.Now().UTC().Add(time.Hour)
	if err := Save(Entry{
		HubURL: "https://worker.test", Role: "worker", DeviceID: "dev",
		Credential: "worker", ExpiresAt: expires, UpdatedAt: time.Now().UTC().Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if err := Save(Entry{
		HubURL: "https://control.test", Role: "control", DeviceID: "dev",
		Credential: "control", ExpiresAt: expires, UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	got, ok := LoadLatest("", "")
	if !ok {
		t.Fatal("expected latest credential")
	}
	if got.Role != "control" || got.Credential != "control" {
		t.Fatalf("unexpected latest credential: %+v", got)
	}
}
