package control

import (
	"os"
	"testing"
	"time"
)

func TestCredentialCacheEntryRoundTrip(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	entry := credentialCacheEntry{
		HubURL: "http://hub.test", Credential: "amx_cred_test", CredentialID: "cred_test",
		TenantID: "tenant_test", Role: "control", DeviceID: "dev_test",
		DeviceName: "laptop", ExpiresAt: time.Now().UTC().Add(time.Hour), UpdatedAt: time.Now().UTC(),
	}
	if err := saveCredentialCacheEntry(entry); err != nil {
		t.Fatal(err)
	}
	got, ok := loadCredentialCacheEntry("http://hub.test")
	if !ok {
		t.Fatal("expected cached credential")
	}
	if got.Credential != entry.Credential || got.DeviceID != entry.DeviceID {
		t.Fatalf("unexpected cache entry: %+v", got)
	}
	path, err := credentialCachePath()
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

func TestCredentialCacheIgnoresExpiredEntry(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	entry := credentialCacheEntry{
		HubURL: "http://hub.test", Credential: "amx_cred_test",
		Role: "control", DeviceID: "dev_test", ExpiresAt: time.Now().UTC().Add(-time.Minute),
		UpdatedAt: time.Now().UTC(),
	}
	if err := saveCredentialCacheEntry(entry); err != nil {
		t.Fatal(err)
	}
	if _, ok := loadCredentialCacheEntry("http://hub.test"); ok {
		t.Fatal("expired credential should not load")
	}
}
