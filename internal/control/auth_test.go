package control

import (
	"testing"
	"time"

	"private/agentmux/internal/credentialcache"
)

func TestCredentialCacheEntryRoundTrip(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	entry := credentialcache.Entry{
		HubURL: "http://hub.test", Credential: "amx_cred_test", CredentialID: "cred_test",
		TenantID: "tenant_test", Role: "control", DeviceID: "dev_test",
		DeviceName: "laptop", ExpiresAt: time.Now().UTC().Add(time.Hour), UpdatedAt: time.Now().UTC(),
	}
	if err := credentialcache.Save(entry); err != nil {
		t.Fatal(err)
	}
	got, ok := credentialcache.Load("http://hub.test", "control", "")
	if !ok {
		t.Fatal("expected cached credential")
	}
	if got.Credential != entry.Credential || got.DeviceID != entry.DeviceID {
		t.Fatalf("unexpected cache entry: %+v", got)
	}
}

func TestCredentialCacheIgnoresExpiredEntry(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	entry := credentialcache.Entry{
		HubURL: "http://hub.test", Credential: "amx_cred_test",
		Role: "control", DeviceID: "dev_test", ExpiresAt: time.Now().UTC().Add(-time.Minute),
		UpdatedAt: time.Now().UTC(),
	}
	if err := credentialcache.Save(entry); err != nil {
		t.Fatal(err)
	}
	if _, ok := credentialcache.Load("http://hub.test", "control", ""); ok {
		t.Fatal("expired credential should not load")
	}
}
