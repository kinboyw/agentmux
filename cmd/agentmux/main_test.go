package main

import (
	"encoding/json"
	"flag"
	"os"
	"testing"
	"time"

	"private/agentmux/internal/appconfig"
	"private/agentmux/internal/credentialcache"
)

func TestIsTUIInvocationRecognizesDirectTUIBinaries(t *testing.T) {
	originalArgs := os.Args
	t.Cleanup(func() { os.Args = originalArgs })

	tests := []struct {
		name string
		arg0 string
		want bool
	}{
		{name: "direct tui", arg0: "/usr/local/bin/agentmux-tui", want: true},
		{name: "platform tui asset", arg0: "./agentmux-tui-linux-amd64", want: true},
		{name: "compatible control asset", arg0: "./agentmux-control-darwin-arm64", want: true},
		{name: "regular cli", arg0: "./agentmux", want: false},
		{name: "hub", arg0: "./agentmux-hub", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			os.Args = []string{tt.arg0}
			if got := isTUIInvocation(); got != tt.want {
				t.Fatalf("isTUIInvocation() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestRoleExecArgsUsesTUIEntrypointForControl(t *testing.T) {
	args := roleExecArgs("control", []string{"--hub", "https://hub.test"})
	if len(args) != 2 || args[0] != "--hub" || args[1] != "https://hub.test" {
		t.Fatalf("unexpected control args: %#v", args)
	}
}

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

func TestWorkerAuthFromCacheEntryPreservesRefreshToken(t *testing.T) {
	expiresAt := time.Now().UTC().Add(time.Hour)
	refreshExpiresAt := time.Now().UTC().Add(24 * time.Hour)
	auth := workerAuthFromCacheEntry(credentialcache.Entry{
		HubURL: "https://hub.test", Credential: "amx_cred_worker", CredentialID: "cred_worker",
		TenantID: "tenant_test", Role: "worker", DeviceID: "worker-1", DeviceName: "Worker One",
		ExpiresAt: expiresAt, RefreshToken: "amx_ref_worker", RefreshExpiresAt: refreshExpiresAt,
	})
	if auth.Source != "cache" || auth.Token != "amx_cred_worker" || auth.RefreshToken != "amx_ref_worker" {
		t.Fatalf("cached worker auth lost credential fields: %+v", auth)
	}
	if !auth.ExpiresAt.Equal(expiresAt) || !auth.RefreshExpiresAt.Equal(refreshExpiresAt) {
		t.Fatalf("cached worker auth lost expiry fields: %+v", auth)
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

func TestDefaultWorkerIDIncludesStableInstanceSuffix(t *testing.T) {
	got := defaultWorkerID("My WSL Worker", "wins_0123456789abcdef")
	if got != "my-wsl-worker-89abcdef" {
		t.Fatalf("unexpected default worker id: %q", got)
	}
}

func TestDefaultWorkerIDSanitizesEmptyName(t *testing.T) {
	got := defaultWorkerID(" / ", "wins_deadbeef")
	if got != "worker-deadbeef" {
		t.Fatalf("unexpected fallback worker id: %q", got)
	}
}

func TestParseP2PICEServersCommaSeparated(t *testing.T) {
	servers, err := parseP2PICEServers("stun:stun1.example.net:3478, turn:turn.example.net:3478?transport=udp")
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 1 || len(servers[0].URLs) != 2 {
		t.Fatalf("unexpected parsed servers: %+v", servers)
	}
}

func TestParseP2PICEServersJSON(t *testing.T) {
	raw, err := json.Marshal([]map[string]any{{
		"urls":       []string{"turns:turn.example.net:5349"},
		"username":   "user1",
		"credential": "secret1",
	}})
	if err != nil {
		t.Fatal(err)
	}
	servers, err := parseP2PICEServers(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 1 || len(servers[0].URLs) != 1 || servers[0].Username != "user1" || servers[0].Credential != "secret1" {
		t.Fatalf("unexpected parsed JSON servers: %+v", servers)
	}
}

func TestAddRuntimeDebugPprofAddrDefaultsDisabled(t *testing.T) {
	fs := flag.NewFlagSet("worker", flag.ContinueOnError)
	debug := addRuntimeDebug(fs, "worker")
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if debug.pprofAddr != "" {
		t.Fatalf("pprof should default disabled, got %q", debug.pprofAddr)
	}
}

func TestAddRuntimeDebugPprofAddrUsesComponentEnv(t *testing.T) {
	t.Setenv("AGENTMUX_PPROF_ADDR", "127.0.0.1:6060")
	t.Setenv("AGENTMUX_WORKER_PPROF_ADDR", "127.0.0.1:6061")

	fs := flag.NewFlagSet("worker", flag.ContinueOnError)
	debug := addRuntimeDebug(fs, "worker")
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if debug.pprofAddr != "127.0.0.1:6061" {
		t.Fatalf("component env should win, got %q", debug.pprofAddr)
	}
}

func TestAddRuntimeDebugPprofAddrFlagOverridesEnv(t *testing.T) {
	t.Setenv("AGENTMUX_WORKER_PPROF_ADDR", "127.0.0.1:6061")

	fs := flag.NewFlagSet("worker", flag.ContinueOnError)
	debug := addRuntimeDebug(fs, "worker")
	if err := fs.Parse([]string{"--pprof-addr", "127.0.0.1:0"}); err != nil {
		t.Fatal(err)
	}
	if debug.pprofAddr != "127.0.0.1:0" {
		t.Fatalf("flag should override env, got %q", debug.pprofAddr)
	}
}
