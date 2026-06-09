package control

import (
	"context"
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

func TestCredentialCacheLoadsExpiredAccessWithValidRefresh(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	entry := credentialcache.Entry{
		HubURL: "http://hub.test", Credential: "amx_cred_test",
		Role: "control", DeviceID: "dev_test", ExpiresAt: time.Now().UTC().Add(-time.Minute),
		RefreshToken: "amx_ref_test", RefreshExpiresAt: time.Now().UTC().Add(24 * time.Hour),
		UpdatedAt: time.Now().UTC(),
	}
	if err := credentialcache.Save(entry); err != nil {
		t.Fatal(err)
	}
	got, ok := credentialcache.Load("http://hub.test", "control", "")
	if !ok {
		t.Fatal("valid refresh token should keep cache entry loadable")
	}
	if got.RefreshToken != entry.RefreshToken {
		t.Fatalf("unexpected cache entry: %+v", got)
	}
}

func TestResolveAppAuthLoadsLatestControlCredentialWithoutHub(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	expires := time.Now().UTC().Add(time.Hour)
	if err := credentialcache.Save(credentialcache.Entry{
		HubURL: "https://hub.example.com", Credential: "amx_cred_control", CredentialID: "cred_control",
		TenantID: "tenant_control", Role: "control", DeviceID: "dev_control",
		DeviceName: "laptop", ExpiresAt: expires, UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	auth, err := ResolveAppAuth(context.Background(), AppAuthOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if auth.Source != "cache" || auth.Client.HubURL != "https://hub.example.com" || auth.Client.Token != "amx_cred_control" {
		t.Fatalf("unexpected auth result: %+v", auth)
	}
}

func TestResolveAppAuthLoadsLatestControlCredentialBeforeLogin(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	expires := time.Now().UTC().Add(time.Hour)
	if err := credentialcache.Save(credentialcache.Entry{
		HubURL: "https://hub.example.com", Credential: "amx_cred_control", CredentialID: "cred_control",
		TenantID: "tenant_control", Role: "control", DeviceID: "dev_control",
		DeviceName: "laptop", ExpiresAt: expires, UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	auth, err := ResolveAppAuth(context.Background(), AppAuthOptions{Login: true})
	if err != nil {
		t.Fatal(err)
	}
	if auth.Source != "cache" || auth.Client.HubURL != "https://hub.example.com" || auth.Client.Token != "amx_cred_control" {
		t.Fatalf("expected cached auth before login, got: %+v", auth)
	}
}

func TestPollIntervalUsesServerInterval(t *testing.T) {
	if got := pollInterval(5, 2*time.Second); got != 5*time.Second {
		t.Fatalf("expected server interval, got %s", got)
	}
	if got := pollInterval(0, 2*time.Second); got != 2*time.Second {
		t.Fatalf("expected fallback interval, got %s", got)
	}
}
