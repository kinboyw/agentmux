package control

import (
	"context"
	"testing"
	"time"

	"private/agentmux/internal/appconfig"
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

func TestDefaultHubURLUsesSavedControlHub(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("AGENTMUX_CONTROL_HUB", "")
	t.Setenv("AGENTMUX_HUB", "")
	if err := appconfig.SaveControlHubURL(" wss://hub.example.com/path?token=secret "); err != nil {
		t.Fatal(err)
	}
	if got := DefaultHubURL(); got != "https://hub.example.com" {
		t.Fatalf("unexpected default hub: %q", got)
	}
}

func TestDefaultHubURLUsesSystemDefaultWhenUnconfigured(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("AGENTMUX_CONTROL_HUB", "")
	t.Setenv("AGENTMUX_HUB", "")
	if got := DefaultHubURL(); got != SystemDefaultHubURL {
		t.Fatalf("unexpected default hub: %q", got)
	}
}

func TestRememberHubURLNormalizesAndSaves(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := RememberHubURL("wss://hub.example.com/ws/"); err != nil {
		t.Fatal(err)
	}
	cfg, err := appconfig.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ControlHubURL != "https://hub.example.com" {
		t.Fatalf("unexpected saved hub: %q", cfg.ControlHubURL)
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
