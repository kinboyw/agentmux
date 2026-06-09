package worker

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"private/agentmux/internal/credentialcache"
)

func TestWorkerURL(t *testing.T) {
	got, err := workerURL("https://agents.example.com", "secret")
	if err != nil {
		t.Fatal(err)
	}
	want := "wss://agents.example.com/ws/worker?token=secret"
	if got != want {
		t.Fatalf("unexpected url:\n got: %s\nwant: %s", got, want)
	}
}

func TestResolveAuthSavesJoinCredential(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	expires := time.Now().UTC().Add(time.Hour)
	oldClient := http.DefaultClient
	t.Cleanup(func() { http.DefaultClient = oldClient })
	http.DefaultClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/exchange" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		return &http.Response{
			StatusCode: http.StatusCreated,
			Status:     "201 Created",
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"credential":"amx_cred_worker","credential_id":"cred_worker","tenant_id":"tenant_test","role":"worker","device_id":"dev_server","expires_at":"` + expires.Format(time.RFC3339Nano) + `","scopes":["worker"]}`)),
			Request:    r,
		}, nil
	})}

	auth, err := ResolveAuth(context.Background(), AuthOptions{
		HubURL: "https://hub.test", Join: "amx_sig_test", DeviceID: "dev_local", DeviceName: "laptop",
	})
	if err != nil {
		t.Fatal(err)
	}
	if auth.Source != "join" || auth.Token != "amx_cred_worker" || auth.DeviceID != "dev_server" {
		t.Fatalf("unexpected auth: %+v", auth)
	}
	cached, ok := credentialcache.Load("https://hub.test", "worker", "dev_server")
	if !ok {
		t.Fatal("expected cached worker credential")
	}
	if cached.Credential != "amx_cred_worker" || cached.CredentialID != "cred_worker" {
		t.Fatalf("unexpected cached credential: %+v", cached)
	}
}

func TestResolveAuthLoadsCachedCredential(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := credentialcache.Save(credentialcache.Entry{
		HubURL: "https://hub.test", Role: "worker", DeviceID: "dev_cached",
		DeviceName: "cached", Credential: "amx_cred_cached", CredentialID: "cred_cached",
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	auth, err := ResolveAuth(context.Background(), AuthOptions{
		HubURL: "wss://hub.test/ws/worker", DeviceID: "dev_cached",
	})
	if err != nil {
		t.Fatal(err)
	}
	if auth.Source != "cache" || auth.Token != "amx_cred_cached" || auth.DeviceName != "cached" {
		t.Fatalf("unexpected auth from cache: %+v", auth)
	}
}

func TestResolveAuthLoadsLatestCachedCredentialWithoutHub(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := credentialcache.Save(credentialcache.Entry{
		HubURL: "https://hub.test", Role: "worker", DeviceID: "dev_cached",
		DeviceName: "cached", Credential: "amx_cred_cached",
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	auth, err := ResolveAuth(context.Background(), AuthOptions{DeviceID: "dev_cached"})
	if err != nil {
		t.Fatal(err)
	}
	if auth.HubURL != "https://hub.test" || auth.Token != "amx_cred_cached" {
		t.Fatalf("unexpected auth from cache: %+v", auth)
	}
}

func TestResolveAuthLoadsLatestCachedCredentialWithoutDevice(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := credentialcache.Save(credentialcache.Entry{
		HubURL: "https://hub.test", Role: "worker", DeviceID: "dev_cached",
		DeviceName: "cached", Credential: "amx_cred_cached",
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	auth, err := ResolveAuth(context.Background(), AuthOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if auth.DeviceID != "dev_cached" || auth.Token != "amx_cred_cached" {
		t.Fatalf("unexpected auth from cache: %+v", auth)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
