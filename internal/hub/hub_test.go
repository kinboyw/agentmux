package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"private/agentmux/internal/protocol"
)

func TestAuthorized(t *testing.T) {
	server := New(":0", "secret", nil)
	req := httptest.NewRequest(http.MethodGet, "/api/sessions?token=secret", nil)
	if !server.authorized(req) {
		t.Fatal("query token should authorize")
	}
	req = httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	req.Header.Set("Authorization", "Bearer secret")
	if !server.authorized(req) {
		t.Fatal("bearer token should authorize")
	}
	req = httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	if server.authorized(req) {
		t.Fatal("missing token should not authorize")
	}
	registered, err := server.auth.Register(registerRequest{
		Email:      "user@example.com",
		Password:   "password123",
		Name:       "User",
		DeviceName: "browser",
	})
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	req.Header.Set("Authorization", "Bearer "+registered.Credential)
	if !server.authorized(req) {
		t.Fatal("registered credential should authorize")
	}
	if !server.authorizedRole(req, "control") {
		t.Fatal("control credential should authorize control routes")
	}
	if server.authorizedRole(req, "worker") {
		t.Fatal("control credential should not authorize worker routes")
	}

	minted, err := server.auth.MintSignal(time.Minute, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	req.Header.Set("Authorization", "Bearer "+minted.Signal)
	if server.authorized(req) {
		t.Fatal("raw signal should not authorize normal APIs")
	}
}

func TestRemoteHost(t *testing.T) {
	if got := remoteHost("127.0.0.1:1234"); got != "127.0.0.1" {
		t.Fatalf("unexpected host: %q", got)
	}
}

func TestHandleControlPage(t *testing.T) {
	server := New(":0", "secret", nil)
	req := httptest.NewRequest(http.MethodGet, "/control?token=secret", nil)
	req.Host = "agentmux.test"
	rec := httptest.NewRecorder()

	server.handleControlPage(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "AgentMux Control") {
		t.Fatalf("control page title missing: %s", body)
	}
	if !strings.Contains(body, "/assets/") {
		t.Fatalf("embedded control assets missing: %s", body)
	}
}

func TestHandleVersion(t *testing.T) {
	server, err := NewWithOptions(ServerOptions{Addr: ":0", ReleaseRepo: "owner/repo"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/version", nil)
	rec := httptest.NewRecorder()

	server.handleVersion(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d", rec.Code)
	}
	var payload map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload["role"] != "hub" || payload["version"] == "" || payload["release_repo"] != "owner/repo" || payload["protocol_version"] != protocol.ProtocolVersion {
		t.Fatalf("unexpected version payload: %#v", payload)
	}
	if _, ok := payload["compatibility"].(map[string]any); !ok {
		t.Fatalf("compatibility payload missing: %#v", payload)
	}
}

func TestHandleAuthOAuthConfiguredProviderReturnsAuthorizeURL(t *testing.T) {
	server, err := NewWithOptions(ServerOptions{
		Addr:      ":0",
		PublicURL: "https://mux.example.com",
		OAuth: map[string]oauthProviderConfig{
			"github": {
				ClientID:     "client-id",
				ClientSecret: "client-secret",
				AuthURL:      "https://github.example/authorize",
				TokenURL:     "https://github.example/token",
				UserURL:      "https://github.example/user",
				Scopes:       []string{"read:user", "user:email"},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/auth/oauth/github?device_id=browser-1", nil)
	rec := httptest.NewRecorder()

	server.handleAuthOAuth(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(payload.URL)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	if query.Get("client_id") != "client-id" {
		t.Fatalf("unexpected oauth url: %s", payload.URL)
	}
	if query.Get("redirect_uri") != "https://mux.example.com/api/auth/oauth/github/callback" {
		t.Fatalf("unexpected redirect uri: %s", query.Get("redirect_uri"))
	}
	if query.Get("scope") != "read:user user:email" || query.Get("state") == "" {
		t.Fatalf("missing scope or state: %s", payload.URL)
	}
}

func TestExternalLoginDoesNotReusePasswordUserTenant(t *testing.T) {
	store := newAuthStore()
	passwordUser, err := store.Register(registerRequest{
		Email: "user@example.com", Password: "password123", DeviceID: "browser-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	externalUser, err := store.LoginExternal(externalLoginRequest{
		Provider: "google", Subject: "google-subject-1", Email: "user@example.com", Name: "User", DeviceID: "browser-2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if externalUser.TenantID == passwordUser.TenantID {
		t.Fatalf("oauth login reused password tenant: %s", externalUser.TenantID)
	}
	if externalUser.User.Email != "user@example.com" {
		t.Fatalf("oauth login should expose display email, got %q", externalUser.User.Email)
	}
}

func TestSQLiteExternalLoginDoesNotReusePasswordUserTenant(t *testing.T) {
	store, err := OpenSQLiteAuthStore(filepath.Join(t.TempDir(), "agentmux.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	passwordUser, err := store.Register(registerRequest{
		Email: "user@example.com", Password: "password123", DeviceID: "browser-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	externalUser, err := store.LoginExternal(externalLoginRequest{
		Provider: "google", Subject: "google-subject-1", Email: "user@example.com", Name: "User", DeviceID: "browser-2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if externalUser.TenantID == passwordUser.TenantID {
		t.Fatalf("oauth login reused password tenant: %s", externalUser.TenantID)
	}
	if externalUser.User.Email != "user@example.com" {
		t.Fatalf("oauth login should expose display email, got %q", externalUser.User.Email)
	}
}

func TestReapExpiredAnonymousWorkersKeepsActiveDirectToken(t *testing.T) {
	server, err := NewWithOptions(ServerOptions{Addr: ":0"})
	if err != nil {
		t.Fatal(err)
	}
	minted, err := server.auth.MintSignalForTenant("", defaultSignalTTL, 1, []string{"control:join"})
	if err != nil {
		t.Fatal(err)
	}
	credential, err := server.auth.Exchange(exchangeRequest{Signal: minted.Signal, Role: "control", DeviceName: "direct-share"})
	if err != nil {
		t.Fatal(err)
	}
	record := workerRecord{id: "anon-worker", tenantID: credential.TenantID, name: "anon", lastSeen: time.Now().UTC(), connected: true}
	server.workerViews[record.id] = record
	if err := server.runtime.SaveWorker(record); err != nil {
		t.Fatal(err)
	}

	reaped, err := server.reapExpiredAnonymousWorkers(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if reaped != 0 {
		t.Fatalf("active direct token should keep anonymous worker, reaped=%d", reaped)
	}
	if _, ok := server.workerViews[record.id]; !ok {
		t.Fatal("worker record should remain while direct token is active")
	}
}

func TestReapExpiredAnonymousWorkersDefersLiveWorker(t *testing.T) {
	server, err := NewWithOptions(ServerOptions{Addr: ":0"})
	if err != nil {
		t.Fatal(err)
	}
	minted, err := server.auth.MintSignalForTenant("", defaultSignalTTL, 1, []string{"control:join"})
	if err != nil {
		t.Fatal(err)
	}
	credential, err := server.auth.Exchange(exchangeRequest{Signal: minted.Signal, Role: "control", DeviceName: "direct-share"})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	worker := &workerConn{id: "anon-worker", tenantID: credential.TenantID, name: "anon", done: done}
	record := workerRecord{id: worker.id, tenantID: worker.tenantID, name: worker.name, lastSeen: time.Now().UTC(), connected: true}
	server.workers[worker.id] = worker
	server.workerViews[worker.id] = record
	if err := server.runtime.SaveWorker(record); err != nil {
		t.Fatal(err)
	}

	reaped, err := server.reapExpiredAnonymousWorkers(time.Now().UTC().Add(defaultCredentialTTL + time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if reaped != 0 {
		t.Fatalf("live worker should defer anonymous reap, got %d", reaped)
	}
	if _, ok := server.workerViews[worker.id]; !ok {
		t.Fatal("live worker view should remain")
	}
	if _, ok := server.workers[worker.id]; !ok {
		t.Fatal("live worker connection should remain")
	}
	select {
	case <-done:
		t.Fatal("live worker should not be interrupted")
	default:
	}
}

func TestSQLiteDeleteTenantRuntimeRemovesWorkerAndUpdateState(t *testing.T) {
	store, err := OpenSQLiteAuthStore(t.TempDir() + "/agentmux.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	tenantID := "anon_sqlite"
	if err := store.SaveWorker(workerRecord{id: "worker-sqlite", tenantID: tenantID, name: "worker", lastSeen: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	job := workerUpdateJob{
		ID: "upd_sqlite", TenantID: tenantID, WorkerID: "worker-sqlite",
		TargetVersion: "latest", Repo: "owner/repo", Status: "queued",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := store.SaveUpdateJob(job); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendUpdateEvent(workerUpdateEvent{ID: "evt_sqlite", JobID: job.ID, TenantID: tenantID, WorkerID: job.WorkerID, Status: "queued", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteTenantRuntime(tenantID); err != nil {
		t.Fatal(err)
	}
	workers, err := store.LoadWorkers()
	if err != nil {
		t.Fatal(err)
	}
	if len(workers) != 0 {
		t.Fatalf("expected no workers after tenant delete, got %+v", workers)
	}
	jobs, err := store.LoadUpdateJobs()
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Fatalf("expected no update jobs after tenant delete, got %+v", jobs)
	}
}

func TestLandingPageIncludesOpenSourceIdentityAndBilingualVisuals(t *testing.T) {
	server := New(":0", "", nil)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "agentmux.test"
	rec := httptest.NewRecorder()

	server.handleLanding(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"https://github.com/kinboyw/agentmux",
		"latestReleaseAPI",
		"ghcr.io/kinboyw/agentmux",
		`class="github-icon"`,
		`id="version-link"`,
		`data-lang="zh"`,
		`data-full="/docassets/system-architecture.png"`,
		`id="lightbox"`,
		"Current Hub",
		"Worker side",
		"Control side",
		"Self-host Hub",
		"Run Hub with binary",
		"Run Hub with Docker",
		"Optional Docker persistence",
		"Optional tunnel when Hub has no public URL",
		"把所有 agent 会话收回到一个 Hub。",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("landing page missing %q", want)
		}
	}
}

func TestSignalIncludesWorkerJoinCommand(t *testing.T) {
	server := New(":0", "secret", nil)
	req := httptest.NewRequest(http.MethodPost, "/api/signals", nil)
	req.Host = "agentmux.test"
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()

	server.handleSignals(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("unexpected status: %d", rec.Code)
	}
	var payload map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	controlURL, ok := payload["control_url"].(string)
	if !ok || controlURL != "http://agentmux.test/control" {
		t.Fatalf("unexpected control_url: %#v", payload["control_url"])
	}
	signal, ok := payload["signal"].(string)
	if !ok || !strings.HasPrefix(signal, "amx_sig_") {
		t.Fatalf("unexpected signal: %#v", payload["signal"])
	}
	if got := payload["worker_command"].(string); !strings.Contains(got, "curl -fsSL http://agentmux.test/install.sh") || strings.Contains(got, "go run") {
		t.Fatalf("unexpected worker command: %s", got)
	}
	if got := payload["worker_join_command"].(string); got != "agentmux worker join --hub 'ws://agentmux.test' --join "+shellQuote(signal)+" --name \"$(hostname)\"" {
		t.Fatalf("unexpected installed worker join command: %s", got)
	}
	if got := payload["control_command"].(string); got != "agentmux-tui --hub 'http://agentmux.test'" {
		t.Fatalf("unexpected control command: %s", got)
	}
	directToken, ok := payload["direct_token"].(string)
	if !ok || !strings.HasPrefix(directToken, "amx_cred_") {
		t.Fatalf("unexpected direct token: %#v", payload["direct_token"])
	}
	shareURL, ok := payload["control_share_url"].(string)
	if !ok || !strings.HasPrefix(shareURL, "http://agentmux.test/control?token=amx_cred_") {
		t.Fatalf("unexpected control share URL: %#v", payload["control_share_url"])
	}
	if got := payload["control_direct_command"].(string); !strings.Contains(got, "agentmux-tui --hub 'http://agentmux.test' --token 'amx_cred_") {
		t.Fatalf("unexpected direct control command: %s", got)
	}
	controlReq := httptest.NewRequest(http.MethodGet, "/api/workers", nil)
	controlReq.Header.Set("Authorization", "Bearer "+directToken)
	if !server.authorizedRole(controlReq, "control") {
		t.Fatal("direct token should authorize control routes")
	}
}

func TestAnonymousSignalIncludesDirectToken(t *testing.T) {
	server := New(":0", "secret", nil)
	req := httptest.NewRequest(http.MethodPost, "/api/signals", nil)
	req.Host = "agentmux.test"
	rec := httptest.NewRecorder()

	server.handleSignals(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	var payload map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	tenantID, ok := payload["tenant_id"].(string)
	if !ok || !strings.HasPrefix(tenantID, "anon_") {
		t.Fatalf("expected anonymous tenant, got %#v", payload["tenant_id"])
	}
	signal, ok := payload["signal"].(string)
	if !ok || !strings.HasPrefix(signal, "amx_sig_") {
		t.Fatalf("unexpected signal: %#v", payload["signal"])
	}
	directToken, ok := payload["direct_token"].(string)
	if !ok || !strings.HasPrefix(directToken, "amx_cred_") {
		t.Fatalf("unexpected direct token: %#v", payload["direct_token"])
	}
	shareURL, ok := payload["control_share_url"].(string)
	if !ok || !strings.Contains(shareURL, "/control?token=amx_cred_") {
		t.Fatalf("unexpected control share URL: %#v", payload["control_share_url"])
	}

	workerCredential, err := server.auth.Exchange(exchangeRequest{Signal: signal, Role: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	if workerCredential.TenantID != tenantID {
		t.Fatalf("expected worker tenant %s, got %s", tenantID, workerCredential.TenantID)
	}
	directReq := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	directReq.Header.Set("Authorization", "Bearer "+directToken)
	directRec := httptest.NewRecorder()
	server.requireAuth(server.handleAuthMe)(directRec, directReq)
	if directRec.Code != http.StatusOK {
		t.Fatalf("direct token should authorize auth/me: %d body=%s", directRec.Code, directRec.Body.String())
	}
}

func TestInvalidSignalCredentialDoesNotFallBackToAnonymous(t *testing.T) {
	server := New(":0", "secret", nil)
	req := httptest.NewRequest(http.MethodPost, "/api/signals", nil)
	req.Header.Set("Authorization", "Bearer not-a-valid-token")
	rec := httptest.NewRecorder()

	server.handleSignals(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestDirectTokenIsLimitedControlAccess(t *testing.T) {
	server := New(":0", "secret", nil)
	signalReq := httptest.NewRequest(http.MethodPost, "/api/signals", nil)
	signalReq.Host = "agentmux.test"
	signalReq.Header.Set("Authorization", "Bearer secret")
	signalRec := httptest.NewRecorder()
	server.handleSignals(signalRec, signalReq)
	if signalRec.Code != http.StatusCreated {
		t.Fatalf("unexpected signal status: %d body=%s", signalRec.Code, signalRec.Body.String())
	}
	var signalPayload map[string]any
	if err := json.NewDecoder(signalRec.Body).Decode(&signalPayload); err != nil {
		t.Fatal(err)
	}
	directToken := signalPayload["direct_token"].(string)
	tenantID := signalPayload["tenant_id"].(string)
	worker := &workerConn{id: "local", tenantID: tenantID, name: "Local", backend: "pty", send: make(chan protocol.Envelope, 2), done: make(chan struct{})}
	server.registerWorker(worker)
	server.sessions["local/demo"] = protocol.SessionView{ID: "local/demo", TenantID: tenantID, WorkerID: "local", Name: "demo"}

	meReq := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	meReq.Header.Set("Authorization", "Bearer "+directToken)
	meRec := httptest.NewRecorder()
	server.requireAuth(server.handleAuthMe)(meRec, meReq)
	if meRec.Code != http.StatusOK {
		t.Fatalf("unexpected me status: %d body=%s", meRec.Code, meRec.Body.String())
	}
	var me map[string]any
	if err := json.NewDecoder(meRec.Body).Decode(&me); err != nil {
		t.Fatal(err)
	}
	if me["access_mode"] != "direct" {
		t.Fatalf("expected direct access mode, got %#v", me["access_mode"])
	}

	listReq := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	listReq.Header.Set("Authorization", "Bearer "+directToken)
	listRec := httptest.NewRecorder()
	server.requireRole("control", server.handleSessions)(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("direct token should list sessions: %d body=%s", listRec.Code, listRec.Body.String())
	}

	workersReq := httptest.NewRequest(http.MethodGet, "/api/workers", nil)
	workersReq.Header.Set("Authorization", "Bearer "+directToken)
	workersRec := httptest.NewRecorder()
	server.requireRole("control", server.handleWorkers)(workersRec, workersReq)
	if workersRec.Code != http.StatusOK {
		t.Fatalf("direct token should list worker status: %d body=%s", workersRec.Code, workersRec.Body.String())
	}
	var workersPayload struct {
		Workers []protocol.WorkerView `json:"workers"`
	}
	if err := json.NewDecoder(workersRec.Body).Decode(&workersPayload); err != nil {
		t.Fatal(err)
	}
	if len(workersPayload.Workers) != 1 || workersPayload.Workers[0].ID != "local" || workersPayload.Workers[0].Backend != "pty" {
		t.Fatalf("unexpected direct worker list: %+v", workersPayload.Workers)
	}

	updateReq := httptest.NewRequest(http.MethodPost, "/api/workers/local/updates", bytes.NewReader([]byte(`{"version":"latest"}`)))
	updateReq.Header.Set("Authorization", "Bearer "+directToken)
	updateRec := httptest.NewRecorder()
	server.requireRole("control", server.handleWorkerAction)(updateRec, updateReq)
	if updateRec.Code != http.StatusForbidden {
		t.Fatalf("direct token should not update workers: %d body=%s", updateRec.Code, updateRec.Body.String())
	}

	createReq := httptest.NewRequest(http.MethodPost, "/api/sessions", bytes.NewReader([]byte(`{"worker_id":"local","name":"new","cwd":".","command":"bash"}`)))
	createReq.Header.Set("Authorization", "Bearer "+directToken)
	createRec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		server.requireRole("control", server.handleSessions)(createRec, createReq)
		close(done)
	}()
	var requestID string
	select {
	case env := <-worker.send:
		if env.Type != protocol.TypeSessionCreate {
			t.Fatalf("unexpected direct create message: %+v", env)
		}
		requestID = env.ID
	case <-time.After(time.Second):
		t.Fatal("worker did not receive direct token create request")
	}
	reply, err := protocol.NewEnvelope(protocol.TypeSessionCreated, protocol.Session{Name: "new", CWD: ".", Command: "bash", Status: "running"})
	if err != nil {
		t.Fatal(err)
	}
	reply.ID = requestID
	reply.SessionID = "local/new"
	server.handleWorkerMessage(worker, reply)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("direct create request did not complete")
	}
	if createRec.Code != http.StatusCreated {
		t.Fatalf("direct token should create sessions: %d body=%s", createRec.Code, createRec.Body.String())
	}

	joinReq := httptest.NewRequest(http.MethodPost, "/api/signals", nil)
	joinReq.Header.Set("Authorization", "Bearer "+directToken)
	joinRec := httptest.NewRecorder()
	server.handleSignals(joinRec, joinReq)
	if joinRec.Code != http.StatusForbidden {
		t.Fatalf("direct token should not generate join signals: %d body=%s", joinRec.Code, joinRec.Body.String())
	}
}

func TestSignalUsesConfiguredPublicURL(t *testing.T) {
	server, err := NewWithOptions(ServerOptions{Addr: ":0", Token: "secret", PublicURL: "https://mux.example.com/"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/signals", nil)
	req.Host = "internal.local"
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()

	server.handleSignals(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("unexpected status: %d", rec.Code)
	}
	var payload map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if got := payload["control_url"].(string); got != "https://mux.example.com/control" {
		t.Fatalf("unexpected control URL: %s", got)
	}
	if got := payload["worker_command"].(string); !strings.Contains(got, "curl -fsSL https://mux.example.com/install.sh") {
		t.Fatalf("worker command did not use public install script URL: %s", got)
	}
}

func TestInstallScriptEndpoint(t *testing.T) {
	server, err := NewWithOptions(ServerOptions{Addr: ":0", ReleaseRepo: "owner/repo"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/install.sh", nil)
	req.Host = "agentmux.test"
	rec := httptest.NewRecorder()

	server.handleInstallScript(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "agentmux") || !strings.Contains(body, "ws://agentmux.test") {
		t.Fatalf("unexpected install script:\n%s", body)
	}
	if !strings.Contains(body, "REPO=\"${AGENTMUX_REPO:-owner/repo}\"") {
		t.Fatalf("release repo missing from install script:\n%s", body)
	}
	if !strings.Contains(body, "releases/latest/download") {
		t.Fatalf("release download path missing from install script:\n%s", body)
	}
	for _, want := range []string{
		"agentmux-${ASSET_ROLE}-${os}-${arch}",
		"asset=\"${asset_base}.tar.gz\"",
		"ASSET_ROLE=\"tui\"",
		"agentmux-tui",
		"legacy_base=\"agentmux-${os}-${arch}\"",
		"agentmux-hub",
		"usage: install.sh worker|control|hub",
		"verify_sha256",
		"checksum_url=\"${url}.sha256\"",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("install script missing %q:\n%s", want, body)
		}
	}
}

func TestRunScriptEndpoint(t *testing.T) {
	server, err := NewWithOptions(ServerOptions{Addr: ":0", ReleaseRepo: "owner/repo"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/run.sh", nil)
	req.Host = "agentmux.test"
	rec := httptest.NewRecorder()

	server.handleRunScript(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"usage: run.sh worker|control|hub[@version]",
		"AGENTMUX_CACHE_DIR",
		"agentmux-${asset_role}-${os}-${arch}",
		"bin_name=\"agentmux-tui\"",
		"verify_sha256",
		"--hub \"$HUB_HTTP\"",
		"worker --hub \"$HUB_WS\"",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("run script missing %q:\n%s", want, body)
		}
	}
}

func TestDocAssetsEndpoint(t *testing.T) {
	server := New(":0", "", nil)
	req := httptest.NewRequest(http.MethodGet, "/docassets/agentmux-mark.svg", nil)
	rec := httptest.NewRecorder()

	server.handleDocAssets(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "AgentMux mark") {
		t.Fatalf("unexpected doc asset body: %s", rec.Body.String())
	}
}

func TestRootMarkAssetEndpoint(t *testing.T) {
	server := New(":0", "", nil)
	req := httptest.NewRequest(http.MethodGet, "/agentmux-mark.svg", nil)
	rec := httptest.NewRecorder()

	server.handleRootAsset(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "image/svg+xml" {
		t.Fatalf("unexpected content type: %s", got)
	}
	if !strings.Contains(rec.Body.String(), "AgentMux mark") {
		t.Fatalf("unexpected root asset body: %s", rec.Body.String())
	}
}

func TestSignalExchangeCredentialAuthorizesWorkerRole(t *testing.T) {
	server := New(":0", "secret", nil)
	signalReq := httptest.NewRequest(http.MethodPost, "/api/signals", nil)
	signalReq.Host = "agentmux.test"
	signalReq.Header.Set("Authorization", "Bearer secret")
	signalRec := httptest.NewRecorder()
	server.handleSignals(signalRec, signalReq)
	if signalRec.Code != http.StatusCreated {
		t.Fatalf("unexpected signal status: %d", signalRec.Code)
	}
	var signalPayload map[string]any
	if err := json.NewDecoder(signalRec.Body).Decode(&signalPayload); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"signal":"` + signalPayload["signal"].(string) + `","role":"worker","device_name":"test"}`)
	exchangeReq := httptest.NewRequest(http.MethodPost, "/api/exchange", bytes.NewReader(body))
	exchangeRec := httptest.NewRecorder()
	server.handleExchange(exchangeRec, exchangeReq)
	if exchangeRec.Code != http.StatusCreated {
		t.Fatalf("unexpected exchange status: %d body=%s", exchangeRec.Code, exchangeRec.Body.String())
	}
	var exchangePayload map[string]any
	if err := json.NewDecoder(exchangeRec.Body).Decode(&exchangePayload); err != nil {
		t.Fatal(err)
	}
	roleReq := httptest.NewRequest(http.MethodGet, "/api/workers/ws", nil)
	roleReq.Header.Set("Authorization", "Bearer "+exchangePayload["credential"].(string))
	if !server.authorizedRole(roleReq, "worker") {
		t.Fatal("exchanged credential should authorize worker role")
	}
	if server.authorizedRole(roleReq, "control") {
		t.Fatal("worker credential should not authorize control role")
	}
}

func TestRegisterCredentialAuthorizesControlAPI(t *testing.T) {
	server := New(":0", "", nil)
	body := []byte(`{"email":"user@example.com","password":"password123","name":"User","device_name":"browser"}`)
	registerReq := httptest.NewRequest(http.MethodPost, "/api/auth/register", bytes.NewReader(body))
	registerRec := httptest.NewRecorder()
	server.handleAuthRegister(registerRec, registerReq)
	if registerRec.Code != http.StatusCreated {
		t.Fatalf("unexpected register status: %d body=%s", registerRec.Code, registerRec.Body.String())
	}
	var registerPayload map[string]any
	if err := json.NewDecoder(registerRec.Body).Decode(&registerPayload); err != nil {
		t.Fatal(err)
	}
	apiReq := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	apiReq.Header.Set("Authorization", "Bearer "+registerPayload["credential"].(string))
	apiRec := httptest.NewRecorder()
	server.requireRole("control", server.handleSessions)(apiRec, apiReq)
	if apiRec.Code != http.StatusOK {
		t.Fatalf("registered credential did not authorize api: %d body=%s", apiRec.Code, apiRec.Body.String())
	}
}

func TestControlCredentialSeesOnlyTenantWorkers(t *testing.T) {
	server := New(":0", "", nil)
	controlCredA, err := server.auth.Register(registerRequest{
		Email:      "tenant-a@example.com",
		Password:   "password123",
		Name:       "Tenant A",
		DeviceName: "browser",
	})
	if err != nil {
		t.Fatal(err)
	}
	mintedA, err := server.auth.MintSignalForTenant(controlCredA.TenantID, time.Minute, 0, []string{"worker:join"})
	if err != nil {
		t.Fatal(err)
	}
	workerCredA, err := server.auth.Exchange(exchangeRequest{Signal: mintedA.Signal, Role: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	mintedB, err := server.auth.MintSignal(time.Minute, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	workerCredB, err := server.auth.Exchange(exchangeRequest{Signal: mintedB.Signal, Role: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	server.registerWorker(&workerConn{id: "a", tenantID: workerCredA.TenantID, send: make(chan protocol.Envelope, 1)})
	server.registerWorker(&workerConn{id: "b", tenantID: workerCredB.TenantID, send: make(chan protocol.Envelope, 1)})

	req := httptest.NewRequest(http.MethodGet, "/api/workers", nil)
	req.Header.Set("Authorization", "Bearer "+controlCredA.Credential)
	rec := httptest.NewRecorder()
	server.requireRole("control", server.handleWorkers)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d", rec.Code)
	}
	var payload struct {
		Workers []protocol.WorkerView `json:"workers"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Workers) != 1 || payload.Workers[0].ID != "a" {
		t.Fatalf("expected only tenant A worker, got %+v", payload.Workers)
	}
}

func TestWorkerDisconnectKeepsRegisteredWorkerView(t *testing.T) {
	server := New(":0", "secret", nil)
	worker := &workerConn{
		id:       "local",
		tenantID: "tenant_test",
		name:     "Laptop",
		addr:     "127.0.0.1",
		backend:  "tmux",
		software: protocol.WorkerSoftware{
			Version: "v1.2.3", ProtocolVersion: protocol.ProtocolVersion,
			OS: "linux", Arch: "amd64", Capabilities: []string{"session.targets"},
		},
		lastSeen: time.Now().UTC().Add(-time.Minute),
		send:     make(chan protocol.Envelope, 1),
	}
	server.registerWorker(worker)
	server.unregisterWorker(worker)

	req := httptest.NewRequest(http.MethodGet, "/api/workers", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	server.requireRole("control", server.handleWorkers)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d", rec.Code)
	}
	var payload struct {
		Workers []protocol.WorkerView `json:"workers"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Workers) != 1 {
		t.Fatalf("expected disconnected worker to remain visible, got %+v", payload.Workers)
	}
	if payload.Workers[0].ID != "local" || payload.Workers[0].Online || payload.Workers[0].Status != "offline" {
		t.Fatalf("unexpected worker view: %+v", payload.Workers[0])
	}
	if payload.Workers[0].Backend != "tmux" || payload.Workers[0].Software.Version != "v1.2.3" || payload.Workers[0].Software.OS != "linux" {
		t.Fatalf("expected offline worker software inventory to remain visible: %+v", payload.Workers[0])
	}
}

func TestWorkerConnectionConflictRejectsDifferentOnlineInstance(t *testing.T) {
	server := New(":0", "secret", nil)
	server.registerWorker(&workerConn{
		id:         "local",
		instanceID: "wins_a",
		tenantID:   "tenant_test",
		send:       make(chan protocol.Envelope, 1),
		done:       make(chan struct{}),
	})
	if err := server.workerConnectionConflict(&workerConn{id: "local", instanceID: "wins_b"}); err == nil || !strings.Contains(err.Error(), "already connected") {
		t.Fatalf("expected worker id conflict, got %v", err)
	}
	if err := server.workerConnectionConflict(&workerConn{id: "local", instanceID: "wins_a"}); err != nil {
		t.Fatalf("same instance should be allowed to reconnect: %v", err)
	}
}

func TestWorkerActionUpdatesManagementFlags(t *testing.T) {
	server := New(":0", "secret", nil)
	server.registerWorker(&workerConn{
		id:       "local",
		tenantID: "tenant_test",
		name:     "Laptop",
		backend:  "tmux",
		send:     make(chan protocol.Envelope, 1),
	})

	body := bytes.NewReader([]byte(`{"enabled":false,"trace_enabled":true,"debug_enabled":true}`))
	req := httptest.NewRequest(http.MethodPatch, "/api/workers/local", body)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	server.requireRole("control", server.handleWorkerAction)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Worker protocol.WorkerView `json:"worker"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Worker.Enabled || !payload.Worker.TraceEnabled || !payload.Worker.DebugEnabled || payload.Worker.Backend != "tmux" {
		t.Fatalf("unexpected worker payload: %+v", payload.Worker)
	}
}

func TestWorkerUpdateQueuesApplyRequest(t *testing.T) {
	server := New(":0", "secret", nil)
	worker := &workerConn{
		id: "local", tenantID: "tenant_test", name: "Laptop", backend: "tmux",
		software: protocol.WorkerSoftware{
			Version: "v0.0.1", ProtocolVersion: protocol.ProtocolVersion,
			Capabilities: []string{"worker.update.apply"},
		},
		send: make(chan protocol.Envelope, 1), done: make(chan struct{}),
	}
	server.registerWorker(worker)

	req := httptest.NewRequest(http.MethodPost, "/api/workers/local/updates", bytes.NewReader([]byte(`{"version":"v9.0.0"}`)))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	server.requireRole("control", server.handleWorkerAction)(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Job workerUpdateJob `json:"job"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Job.ID == "" || payload.Job.Status != "sent" || payload.Job.TargetVersion != "v9.0.0" {
		t.Fatalf("unexpected update job: %+v", payload.Job)
	}
	select {
	case env := <-worker.send:
		if env.Type != protocol.TypeWorkerUpdateApply || env.ID != payload.Job.ID {
			t.Fatalf("unexpected worker message: %+v", env)
		}
		var apply protocol.WorkerUpdateApply
		if err := env.DecodePayload(&apply); err != nil {
			t.Fatal(err)
		}
		if apply.JobID != payload.Job.ID || apply.Version != "v9.0.0" || !apply.Restart {
			t.Fatalf("unexpected apply payload: %+v", apply)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not receive update apply")
	}
}

func TestWorkerUpdateRejectsPtyWithoutDisruptiveConfirmation(t *testing.T) {
	server := New(":0", "secret", nil)
	server.registerWorker(&workerConn{
		id: "local", backend: "pty",
		software: protocol.WorkerSoftware{Capabilities: []string{"worker.update.apply"}},
		send:     make(chan protocol.Envelope, 1),
	})

	req := httptest.NewRequest(http.MethodPost, "/api/workers/local/updates", bytes.NewReader([]byte(`{"version":"v9.0.0"}`)))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	server.requireRole("control", server.handleWorkerAction)(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected conflict, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestWorkerUpdateResultAndReconnectMarksSucceeded(t *testing.T) {
	server := New(":0", "secret", nil)
	worker := &workerConn{
		id: "local", backend: "tmux",
		software: protocol.WorkerSoftware{
			Version: "v0.0.1", ProtocolVersion: protocol.ProtocolVersion,
			Capabilities: []string{"worker.update.apply"},
		},
		send: make(chan protocol.Envelope, 1), done: make(chan struct{}),
	}
	server.registerWorker(worker)

	req := httptest.NewRequest(http.MethodPost, "/api/workers/local/updates", bytes.NewReader([]byte(`{"version":"v9.0.0"}`)))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	server.requireRole("control", server.handleWorkerAction)(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Job workerUpdateJob `json:"job"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	<-worker.send
	result, err := protocol.NewEnvelope(protocol.TypeWorkerUpdateResult, protocol.WorkerUpdateResult{JobID: payload.Job.ID, Status: "restarting", Version: "v9.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	result.ID = payload.Job.ID
	server.handleWorkerMessage(worker, result)

	server.mu.RLock()
	if got := server.updateJobs[payload.Job.ID].Status; got != "restarting" {
		t.Fatalf("expected restarting, got %s", got)
	}
	server.mu.RUnlock()

	server.registerWorker(&workerConn{
		id: "local", backend: "tmux",
		software: protocol.WorkerSoftware{
			Version: "v9.0.0", ProtocolVersion: protocol.ProtocolVersion,
			Capabilities: []string{"worker.update.apply"},
		},
		send: make(chan protocol.Envelope, 1), done: make(chan struct{}),
	})
	server.mu.RLock()
	job := server.updateJobs[payload.Job.ID]
	server.mu.RUnlock()
	if job.Status != "succeeded" || job.FinishedAt.IsZero() {
		t.Fatalf("expected succeeded job after reconnect: %+v", job)
	}
}

func TestSQLiteRuntimeStatePersistsWorkersAndUpdateJobs(t *testing.T) {
	store, err := OpenSQLiteAuthStore(t.TempDir() + "/agentmux.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := NewWithOptions(ServerOptions{Addr: ":0", Token: "secret", AuthStore: store})
	if err != nil {
		t.Fatal(err)
	}
	worker := &workerConn{
		id: "local", instanceID: "wins_test", tenantID: "tenant_test", name: "Laptop", backend: "tmux",
		software: protocol.WorkerSoftware{
			Version: "v0.0.1", ProtocolVersion: protocol.ProtocolVersion,
			OS: "linux", Arch: "amd64", Capabilities: []string{"worker.update.apply"},
		},
		send: make(chan protocol.Envelope, 1), done: make(chan struct{}),
	}
	server.registerWorker(worker)

	updateReq := httptest.NewRequest(http.MethodPost, "/api/workers/local/updates", bytes.NewReader([]byte(`{"version":"v9.0.0"}`)))
	updateReq.Header.Set("Authorization", "Bearer secret")
	updateRec := httptest.NewRecorder()
	server.requireRole("control", server.handleWorkerAction)(updateRec, updateReq)
	if updateRec.Code != http.StatusAccepted {
		t.Fatalf("unexpected update status: %d body=%s", updateRec.Code, updateRec.Body.String())
	}
	<-worker.send
	server.unregisterWorker(worker)

	restored, err := NewWithOptions(ServerOptions{Addr: ":0", Token: "secret", AuthStore: store})
	if err != nil {
		t.Fatal(err)
	}
	workersReq := httptest.NewRequest(http.MethodGet, "/api/workers", nil)
	workersReq.Header.Set("Authorization", "Bearer secret")
	workersRec := httptest.NewRecorder()
	restored.requireRole("control", restored.handleWorkers)(workersRec, workersReq)
	if workersRec.Code != http.StatusOK {
		t.Fatalf("unexpected workers status: %d body=%s", workersRec.Code, workersRec.Body.String())
	}
	var workersPayload struct {
		Workers []protocol.WorkerView `json:"workers"`
	}
	if err := json.NewDecoder(workersRec.Body).Decode(&workersPayload); err != nil {
		t.Fatal(err)
	}
	if len(workersPayload.Workers) != 1 {
		t.Fatalf("expected one restored worker, got %+v", workersPayload.Workers)
	}
	restoredWorker := workersPayload.Workers[0]
	if restoredWorker.ID != "local" || restoredWorker.InstanceID != "wins_test" || restoredWorker.Online {
		t.Fatalf("unexpected restored worker: %+v", restoredWorker)
	}
	if restoredWorker.Software.Version != "v0.0.1" || restoredWorker.Software.OS != "linux" {
		t.Fatalf("unexpected restored worker software: %+v", restoredWorker.Software)
	}

	jobsReq := httptest.NewRequest(http.MethodGet, "/api/workers/local/updates", nil)
	jobsReq.Header.Set("Authorization", "Bearer secret")
	jobsRec := httptest.NewRecorder()
	restored.requireRole("control", restored.handleWorkerAction)(jobsRec, jobsReq)
	if jobsRec.Code != http.StatusOK {
		t.Fatalf("unexpected jobs status: %d body=%s", jobsRec.Code, jobsRec.Body.String())
	}
	var jobsPayload struct {
		Jobs []workerUpdateJob `json:"jobs"`
	}
	if err := json.NewDecoder(jobsRec.Body).Decode(&jobsPayload); err != nil {
		t.Fatal(err)
	}
	if len(jobsPayload.Jobs) != 1 {
		t.Fatalf("expected one restored update job, got %+v", jobsPayload.Jobs)
	}
	job := jobsPayload.Jobs[0]
	if job.WorkerInstanceID != "wins_test" || job.Status != "sent" || len(job.Events) < 2 {
		t.Fatalf("unexpected restored update job: %+v", job)
	}
}

func TestWorkerEvictDisconnectsAndDisablesWorker(t *testing.T) {
	server := New(":0", "secret", nil)
	worker := &workerConn{
		id: "local", tenantID: "admin", name: "Local", addr: "127.0.0.1",
		send: make(chan protocol.Envelope, 1), done: make(chan struct{}),
		lastSeen: time.Now().UTC(),
	}
	server.registerWorker(worker)
	server.sessions["local/demo"] = protocol.SessionView{ID: "local/demo", WorkerID: "local", Name: "demo"}

	req := httptest.NewRequest(http.MethodDelete, "/api/workers/local", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	server.requireRole("control", server.handleWorkerAction)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	select {
	case <-worker.done:
	default:
		t.Fatal("expected worker connection to be closed")
	}
	server.mu.RLock()
	record := server.workerViews["local"]
	_, connected := server.workers["local"]
	_, sessionExists := server.sessions["local/demo"]
	server.mu.RUnlock()
	if connected || sessionExists || !record.disabled || record.connected {
		t.Fatalf("worker was not evicted: connected=%v session=%v record=%+v", connected, sessionExists, record)
	}
	if server.workerAllowedToConnect(&workerConn{id: "local"}) {
		t.Fatal("evicted worker should not be allowed to reconnect")
	}
}

func TestDisabledWorkerRejectsCreateAndAttach(t *testing.T) {
	server := New(":0", "secret", nil)
	worker := &workerConn{
		id:   "local",
		send: make(chan protocol.Envelope, 1),
	}
	server.registerWorker(worker)
	enabled := false
	server.mu.Lock()
	record := server.workerViews["local"]
	record.disabled = !enabled
	server.workerViews["local"] = record
	server.mu.Unlock()

	createReq := httptest.NewRequest(http.MethodPost, "/api/sessions", bytes.NewReader([]byte(`{"worker_id":"local","name":"demo","cwd":".","command":"bash"}`)))
	createReq.Header.Set("Authorization", "Bearer secret")
	createRec := httptest.NewRecorder()
	server.requireRole("control", server.handleSessions)(createRec, createReq)
	if createRec.Code != http.StatusForbidden {
		t.Fatalf("expected create forbidden, got %d body=%s", createRec.Code, createRec.Body.String())
	}

	control := &controlConn{send: make(chan protocol.Envelope, 1)}
	payload, err := protocol.MarshalPayload(protocol.TerminalSize{Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	server.handleControlMessage(control, protocol.Envelope{
		Type:      protocol.TypeControlOpen,
		StreamID:  "stream-1",
		SessionID: "local/demo",
		Payload:   payload,
	})

	select {
	case env := <-control.send:
		if env.Type != protocol.TypeError {
			t.Fatalf("expected error envelope, got %s", env.Type)
		}
		if !strings.Contains(string(env.Payload), "worker is disabled") {
			t.Fatalf("unexpected error payload: %s", string(env.Payload))
		}
	case <-time.After(time.Second):
		t.Fatal("control did not receive disabled worker error")
	}
	if len(worker.send) != 0 {
		t.Fatal("disabled worker should not receive terminal.open")
	}
}

func TestSessionCreateWaitsForWorkerAck(t *testing.T) {
	server := New(":0", "secret", nil)
	worker := &workerConn{
		id:   "local",
		send: make(chan protocol.Envelope, 1),
	}
	server.registerWorker(worker)

	req := httptest.NewRequest(http.MethodPost, "/api/sessions", bytes.NewReader([]byte(`{"worker_id":"local","name":"demo","cwd":"/tmp/demo","command":"bash"}`)))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		server.requireRole("control", server.handleSessions)(rec, req)
		close(done)
	}()

	var requestID string
	select {
	case env := <-worker.send:
		if env.Type != protocol.TypeSessionCreate {
			t.Fatalf("unexpected type: %s", env.Type)
		}
		requestID = env.ID
		if requestID == "" {
			t.Fatal("session.create should include request id")
		}
		if env.SessionID != "local/demo" {
			t.Fatalf("unexpected session id: %s", env.SessionID)
		}
		var payload protocol.Session
		if err := env.DecodePayload(&payload); err != nil {
			t.Fatal(err)
		}
		if payload.CWD != "/tmp/demo" || payload.Command != "bash" {
			t.Fatalf("unexpected create payload: %+v", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not receive session.create")
	}

	reply, err := protocol.NewEnvelope(protocol.TypeSessionCreated, protocol.Session{Name: "demo", CWD: "/tmp/demo", Command: "bash", Status: "running"})
	if err != nil {
		t.Fatal(err)
	}
	reply.ID = requestID
	reply.SessionID = "local/demo"
	server.handleWorkerMessage(worker, reply)

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("create request did not complete")
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"status":"created"`) {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}
}

func TestSessionCreateReturnsWorkerValidationError(t *testing.T) {
	server := New(":0", "secret", nil)
	worker := &workerConn{
		id:   "local",
		send: make(chan protocol.Envelope, 1),
	}
	server.registerWorker(worker)

	req := httptest.NewRequest(http.MethodPost, "/api/sessions", bytes.NewReader([]byte(`{"worker_id":"local","name":"demo","cwd":"/missing","command":"bash"}`)))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		server.requireRole("control", server.handleSessions)(rec, req)
		close(done)
	}()

	var requestID string
	select {
	case env := <-worker.send:
		if env.Type != protocol.TypeSessionCreate {
			t.Fatalf("unexpected type: %s", env.Type)
		}
		requestID = env.ID
	case <-time.After(time.Second):
		t.Fatal("worker did not receive session.create")
	}

	reply, err := protocol.NewEnvelope(protocol.TypeError, protocol.ErrorPayload{Message: "working directory not found"})
	if err != nil {
		t.Fatal(err)
	}
	reply.ID = requestID
	reply.SessionID = "local/demo"
	server.handleWorkerMessage(worker, reply)

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("create request did not complete")
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "working directory not found") {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}
}

func TestControlCredentialMintsSignalForOwnTenant(t *testing.T) {
	server := New(":0", "", nil)
	body := []byte(`{"email":"user@example.com","password":"password123","name":"User","device_name":"browser"}`)
	registerReq := httptest.NewRequest(http.MethodPost, "/api/auth/register", bytes.NewReader(body))
	registerRec := httptest.NewRecorder()
	server.handleAuthRegister(registerRec, registerReq)
	if registerRec.Code != http.StatusCreated {
		t.Fatalf("unexpected register status: %d body=%s", registerRec.Code, registerRec.Body.String())
	}
	var registerPayload authCredentialResponse
	if err := json.NewDecoder(registerRec.Body).Decode(&registerPayload); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/signals", nil)
	req.Host = "agentmux.test"
	req.Header.Set("Authorization", "Bearer "+registerPayload.Credential)
	rec := httptest.NewRecorder()
	server.handleSignals(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("unexpected signal status: %d body=%s", rec.Code, rec.Body.String())
	}

	var payload map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if got := payload["tenant_id"]; got != registerPayload.TenantID {
		t.Fatalf("expected tenant %s, got %#v", registerPayload.TenantID, got)
	}

	exchanged, err := server.auth.Exchange(exchangeRequest{Signal: payload["signal"].(string), Role: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	if exchanged.TenantID != registerPayload.TenantID {
		t.Fatalf("expected exchanged worker tenant %s, got %s", registerPayload.TenantID, exchanged.TenantID)
	}
	if _, err := server.auth.Exchange(exchangeRequest{Signal: payload["signal"].(string), Role: "control"}); err == nil {
		t.Fatal("expected worker join signal to reject control exchange")
	}
}

func TestDeviceLoginEndpoints(t *testing.T) {
	server := New(":0", "", nil)
	registerBody := []byte(`{"email":"device@example.com","password":"password123","name":"Device","device_name":"browser"}`)
	registerReq := httptest.NewRequest(http.MethodPost, "/api/auth/register", bytes.NewReader(registerBody))
	registerRec := httptest.NewRecorder()
	server.handleAuthRegister(registerRec, registerReq)
	if registerRec.Code != http.StatusCreated {
		t.Fatalf("unexpected register status: %d body=%s", registerRec.Code, registerRec.Body.String())
	}
	var registerPayload authCredentialResponse
	if err := json.NewDecoder(registerRec.Body).Decode(&registerPayload); err != nil {
		t.Fatal(err)
	}

	startBody := []byte(`{"device_id":"cli","device_name":"CLI"}`)
	startReq := httptest.NewRequest(http.MethodPost, "/api/auth/device/start", bytes.NewReader(startBody))
	startReq.Host = "agentmux.test"
	startRec := httptest.NewRecorder()
	server.handleDeviceStart(startRec, startReq)
	if startRec.Code != http.StatusCreated {
		t.Fatalf("unexpected start status: %d body=%s", startRec.Code, startRec.Body.String())
	}
	var start deviceStartResponse
	if err := json.NewDecoder(startRec.Body).Decode(&start); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(start.VerificationURLComplete, "http://agentmux.test/device?user_code=") {
		t.Fatalf("unexpected verification URL: %s", start.VerificationURLComplete)
	}
	pageReq := httptest.NewRequest(http.MethodGet, "/device?user_code="+url.QueryEscape(start.UserCode), nil)
	pageRec := httptest.NewRecorder()
	server.handleDevicePage(pageRec, pageReq)
	if pageRec.Code != http.StatusOK {
		t.Fatalf("unexpected device page status: %d body=%s", pageRec.Code, pageRec.Body.String())
	}
	if pageRec.Header().Get("Referrer-Policy") != "no-referrer" || pageRec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("missing device page security headers: %+v", pageRec.Header())
	}
	if body := pageRec.Body.String(); !strings.Contains(body, "CLI") || !strings.Contains(body, "history.replaceState") || !strings.Contains(body, "Continue with GitHub") || !strings.Contains(body, "approve-current") {
		t.Fatalf("device page missing context or OAuth placeholders: %s", body)
	}

	unauthorizedBody := []byte(`{"user_code":"` + start.UserCode + `"}`)
	unauthorizedReq := httptest.NewRequest(http.MethodPost, "/api/auth/device/approve-current", bytes.NewReader(unauthorizedBody))
	unauthorizedRec := httptest.NewRecorder()
	server.handleDeviceApproveCurrent(unauthorizedRec, unauthorizedReq)
	if unauthorizedRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected unauthorized approve-current status, got %d body=%s", unauthorizedRec.Code, unauthorizedRec.Body.String())
	}

	approveReq := httptest.NewRequest(http.MethodPost, "/api/auth/device/approve-current", bytes.NewReader(unauthorizedBody))
	approveReq.Header.Set("Authorization", "Bearer "+registerPayload.Credential)
	approveRec := httptest.NewRecorder()
	server.handleDeviceApproveCurrent(approveRec, approveReq)
	if approveRec.Code != http.StatusCreated {
		t.Fatalf("unexpected approve status: %d body=%s", approveRec.Code, approveRec.Body.String())
	}

	pollBody := []byte(`{"device_code":"` + start.DeviceCode + `"}`)
	pollReq := httptest.NewRequest(http.MethodPost, "/api/auth/device/poll", bytes.NewReader(pollBody))
	pollRec := httptest.NewRecorder()
	server.handleDevicePoll(pollRec, pollReq)
	if pollRec.Code != http.StatusOK {
		t.Fatalf("unexpected poll status: %d body=%s", pollRec.Code, pollRec.Body.String())
	}
	var poll devicePollResponse
	if err := json.NewDecoder(pollRec.Body).Decode(&poll); err != nil {
		t.Fatal(err)
	}
	if poll.Status != "approved" || poll.Credential == nil || poll.Credential.Role != "control" {
		t.Fatalf("unexpected poll response: %+v", poll)
	}
}

func TestControlOpenForwardsInitialTerminalSize(t *testing.T) {
	server := New(":0", "secret", nil)
	worker := &workerConn{
		id:   "local",
		send: make(chan protocol.Envelope, 1),
	}
	server.registerWorker(worker)
	control := &controlConn{send: make(chan protocol.Envelope, 1)}
	payload, err := protocol.MarshalPayload(protocol.TerminalSize{Cols: 132, Rows: 41})
	if err != nil {
		t.Fatal(err)
	}

	server.handleControlMessage(control, protocol.Envelope{
		Type:      protocol.TypeControlOpen,
		StreamID:  "stream-1",
		SessionID: "local/demo",
		Payload:   payload,
	})

	select {
	case env := <-worker.send:
		if env.Type != protocol.TypeTerminalOpen {
			t.Fatalf("unexpected type: %s", env.Type)
		}
		var size protocol.TerminalSize
		if err := env.DecodePayload(&size); err != nil {
			t.Fatal(err)
		}
		if size.Cols != 132 || size.Rows != 41 {
			t.Fatalf("initial size not forwarded: %+v", size)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not receive terminal.open")
	}
}

func TestControlOpenP2PPreferredIssuesGrantAndSignals(t *testing.T) {
	server := New(":0", "secret", nil)
	worker := &workerConn{
		id:   "local",
		send: make(chan protocol.Envelope, 4),
	}
	server.registerWorker(worker)
	control := &controlConn{id: "ctrl_test", send: make(chan protocol.Envelope, 4)}
	server.addControl(control)
	payload, err := protocol.MarshalPayload(protocol.TerminalOpen{
		Cols:        100,
		Rows:        30,
		ChannelMode: protocol.TerminalChannelP2PPreferred,
	})
	if err != nil {
		t.Fatal(err)
	}

	server.handleControlMessage(control, protocol.Envelope{
		Type:      protocol.TypeControlOpen,
		StreamID:  "stream-1",
		SessionID: "local/demo",
		Payload:   payload,
	})

	var open protocol.TerminalOpen
	select {
	case env := <-worker.send:
		if env.Type != protocol.TypeTerminalOpen {
			t.Fatalf("unexpected worker envelope type: %s", env.Type)
		}
		if err := env.DecodePayload(&open); err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not receive terminal.open")
	}
	if open.ChannelMode != protocol.TerminalChannelP2PPreferred || open.GrantID == "" {
		t.Fatalf("expected p2p terminal.open with grant id, got %+v", open)
	}

	var grant protocol.P2PGrant
	select {
	case env := <-control.send:
		if env.Type != protocol.TypeP2PGrant {
			t.Fatalf("unexpected control envelope type: %s", env.Type)
		}
		if err := env.DecodePayload(&grant); err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("control did not receive p2p grant")
	}
	if grant.GrantID != open.GrantID || grant.WorkerID != "local" || grant.SessionID != "local/demo" || grant.StreamID != "stream-1" {
		t.Fatalf("unexpected grant: %+v open=%+v", grant, open)
	}
	if len(grant.ICEServers) != 0 {
		t.Fatalf("unexpected default ice servers in grant: %+v", grant.ICEServers)
	}

	var firstSignal protocol.P2PSignal
	select {
	case env := <-control.send:
		if env.Type != protocol.TypeP2PSignal {
			t.Fatalf("unexpected first signal type: %s", env.Type)
		}
		if err := env.DecodePayload(&firstSignal); err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("control did not receive p2p signaling placeholder")
	}
	if firstSignal.Signal != "grant_issued" || firstSignal.GrantID != open.GrantID {
		t.Fatalf("unexpected first signal: %+v", firstSignal)
	}

	offer, err := protocol.MarshalPayload(protocol.P2PSignal{GrantID: open.GrantID, From: "control", To: "worker", Signal: "offer_placeholder", State: "negotiating"})
	if err != nil {
		t.Fatal(err)
	}
	server.handleControlMessage(control, protocol.Envelope{
		Type:      protocol.TypeP2PSignal,
		SessionID: "local/demo",
		StreamID:  "stream-1",
		Payload:   offer,
	})

	var forwardedOffer protocol.P2PSignal
	select {
	case env := <-worker.send:
		if env.Type != protocol.TypeP2PSignal {
			t.Fatalf("unexpected forwarded signal type: %s", env.Type)
		}
		if err := env.DecodePayload(&forwardedOffer); err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not receive p2p offer signal")
	}
	if forwardedOffer.Signal != "offer_placeholder" || forwardedOffer.GrantID != open.GrantID || forwardedOffer.To != "worker" {
		t.Fatalf("unexpected forwarded offer: %+v", forwardedOffer)
	}

	unsupported, err := protocol.MarshalPayload(protocol.P2PSignal{
		GrantID: open.GrantID,
		From:    "worker",
		To:      "control",
		Signal:  "unsupported",
		State:   protocol.TerminalChannelP2PFallback,
		Reason:  "worker_direct_transport_not_implemented",
		Message: "Worker direct transport is not implemented yet; continuing over Hub relay.",
	})
	if err != nil {
		t.Fatal(err)
	}
	server.handleWorkerMessage(worker, protocol.Envelope{
		Type:      protocol.TypeP2PSignal,
		SessionID: "local/demo",
		StreamID:  "stream-1",
		Payload:   unsupported,
	})

	var fallback protocol.P2PSignal
	select {
	case env := <-control.send:
		if env.Type != protocol.TypeP2PSignal {
			t.Fatalf("unexpected fallback signal type: %s", env.Type)
		}
		if err := env.DecodePayload(&fallback); err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("control did not receive p2p fallback signal")
	}
	if fallback.Signal != "unsupported" || fallback.State != protocol.TerminalChannelP2PFallback || fallback.Reason != "worker_direct_transport_not_implemented" {
		t.Fatalf("unexpected fallback signal: %+v", fallback)
	}
}

func TestControlDisconnectClosesWorkerStream(t *testing.T) {
	server := New(":0", "secret", nil)
	worker := &workerConn{
		id:   "local",
		send: make(chan protocol.Envelope, 4),
	}
	server.registerWorker(worker)
	control := &controlConn{id: "ctrl_test", send: make(chan protocol.Envelope, 1), done: make(chan struct{})}
	server.addControl(control)

	payload, err := protocol.MarshalPayload(protocol.TerminalOpen{Cols: 100, Rows: 30})
	if err != nil {
		t.Fatal(err)
	}
	server.handleControlMessage(control, protocol.Envelope{
		Type:      protocol.TypeControlOpen,
		StreamID:  "stream-1",
		SessionID: "local/demo",
		Payload:   payload,
	})
	select {
	case env := <-worker.send:
		if env.Type != protocol.TypeTerminalOpen {
			t.Fatalf("unexpected type: %s", env.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not receive terminal.open")
	}

	server.removeControl(control)

	select {
	case env := <-worker.send:
		if env.Type != protocol.TypeTerminalClose {
			t.Fatalf("unexpected type: %s", env.Type)
		}
		if env.StreamID != "stream-1" || env.SessionID != "local/demo" {
			t.Fatalf("unexpected close envelope: %+v", env)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not receive terminal.close")
	}
}

func TestTargetedPaneOpenReplacesPriorControlStream(t *testing.T) {
	server := New(":0", "secret", nil)
	worker := &workerConn{
		id:   "local",
		send: make(chan protocol.Envelope, 8),
	}
	server.registerWorker(worker)
	controlA := &controlConn{id: "ctrl_a", send: make(chan protocol.Envelope, 1), done: make(chan struct{})}
	controlB := &controlConn{id: "ctrl_b", send: make(chan protocol.Envelope, 1), done: make(chan struct{})}
	server.addControl(controlA)
	server.addControl(controlB)

	firstPayload, err := protocol.MarshalPayload(protocol.TerminalOpen{
		Cols: 100,
		Rows: 30,
		Target: &protocol.TerminalTarget{
			SessionName: "demo",
			PaneID:      "%1",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	server.handleControlMessage(controlA, protocol.Envelope{
		Type:      protocol.TypeControlOpen,
		StreamID:  "stream-a",
		SessionID: "local/demo",
		Payload:   firstPayload,
	})
	select {
	case env := <-worker.send:
		if env.Type != protocol.TypeTerminalOpen || env.StreamID != "stream-a" {
			t.Fatalf("unexpected first open envelope: %+v", env)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not receive first terminal.open")
	}

	secondPayload, err := protocol.MarshalPayload(protocol.TerminalOpen{
		Cols: 120,
		Rows: 36,
		Target: &protocol.TerminalTarget{
			SessionName: "demo",
			PaneID:      "%1",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	server.handleControlMessage(controlB, protocol.Envelope{
		Type:      protocol.TypeControlOpen,
		StreamID:  "stream-b",
		SessionID: "local/demo",
		Payload:   secondPayload,
	})

	var openSeen, closeSeen bool
	deadline := time.After(time.Second)
	for !(openSeen && closeSeen) {
		select {
		case env := <-worker.send:
			switch {
			case env.Type == protocol.TypeTerminalClose && env.StreamID == "stream-a":
				closeSeen = true
			case env.Type == protocol.TypeTerminalOpen && env.StreamID == "stream-b":
				openSeen = true
			default:
				t.Fatalf("unexpected envelope after second open: %+v", env)
			}
		case <-deadline:
			t.Fatalf("expected open and close, got open=%t close=%t", openSeen, closeSeen)
		}
	}
}

func TestUntargetedSessionOpenDoesNotReplaceOtherControlStream(t *testing.T) {
	server := New(":0", "secret", nil)
	worker := &workerConn{
		id:   "local",
		send: make(chan protocol.Envelope, 8),
	}
	server.registerWorker(worker)
	controlA := &controlConn{id: "ctrl_a", send: make(chan protocol.Envelope, 1), done: make(chan struct{})}
	controlB := &controlConn{id: "ctrl_b", send: make(chan protocol.Envelope, 1), done: make(chan struct{})}
	server.addControl(controlA)
	server.addControl(controlB)

	payload, err := protocol.MarshalPayload(protocol.TerminalOpen{Cols: 100, Rows: 30})
	if err != nil {
		t.Fatal(err)
	}
	server.handleControlMessage(controlA, protocol.Envelope{
		Type:      protocol.TypeControlOpen,
		StreamID:  "stream-a",
		SessionID: "local/demo",
		Payload:   payload,
	})
	server.handleControlMessage(controlB, protocol.Envelope{
		Type:      protocol.TypeControlOpen,
		StreamID:  "stream-b",
		SessionID: "local/demo",
		Payload:   payload,
	})

	first := <-worker.send
	second := <-worker.send
	if first.Type != protocol.TypeTerminalOpen || second.Type != protocol.TypeTerminalOpen {
		t.Fatalf("expected two terminal.open envelopes, got %+v then %+v", first, second)
	}
	if len(worker.send) != 0 {
		env := <-worker.send
		t.Fatalf("unexpected extra envelope: %+v", env)
	}
}

func TestOrphanTerminalOutputClosesWorkerStream(t *testing.T) {
	server := New(":0", "secret", nil)
	worker := &workerConn{
		id:   "local",
		send: make(chan protocol.Envelope, 4),
	}
	server.registerWorker(worker)

	output, err := protocol.NewEnvelope(protocol.TypeTerminalOutput, protocol.TerminalOutput{Data: "x"})
	if err != nil {
		t.Fatal(err)
	}
	output.SessionID = "local/demo"
	output.StreamID = "orphan-stream"
	server.handleWorkerMessage(worker, output)

	select {
	case env := <-worker.send:
		if env.Type != protocol.TypeTerminalClose {
			t.Fatalf("unexpected type: %s", env.Type)
		}
		if env.StreamID != "orphan-stream" || env.SessionID != "local/demo" {
			t.Fatalf("unexpected close envelope: %+v", env)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not receive orphan terminal.close")
	}
}

func TestSessionPreviewRequestForwardsToWorker(t *testing.T) {
	server := New(":0", "secret", nil)
	worker := &workerConn{
		id:   "local",
		send: make(chan protocol.Envelope, 1),
	}
	server.registerWorker(worker)

	req := httptest.NewRequest(http.MethodGet, "/api/sessions/local/demo/preview?lines=12", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		server.requireRole("control", server.handleSessionAction)(rec, req)
		close(done)
	}()

	var requestID string
	select {
	case env := <-worker.send:
		if env.Type != protocol.TypeSessionPreview {
			t.Fatalf("unexpected type: %s", env.Type)
		}
		requestID = env.ID
		if env.SessionID != "local/demo" {
			t.Fatalf("unexpected session id: %s", env.SessionID)
		}
		var payload protocol.SessionPreviewRequest
		if err := env.DecodePayload(&payload); err != nil {
			t.Fatal(err)
		}
		if payload.Lines != 12 {
			t.Fatalf("unexpected lines: %d", payload.Lines)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not receive preview request")
	}

	reply, err := protocol.NewEnvelope(protocol.TypeSessionPreview, protocol.SessionPreview{Data: "preview"})
	if err != nil {
		t.Fatal(err)
	}
	reply.ID = requestID
	server.handleWorkerMessage(worker, reply)

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("preview request did not complete")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"data":"preview"`) {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}
}

func TestSessionPreviewRequestForwardsTargetToWorker(t *testing.T) {
	server := New(":0", "secret", nil)
	worker := &workerConn{
		id:   "local",
		send: make(chan protocol.Envelope, 1),
	}
	server.registerWorker(worker)

	req := httptest.NewRequest(http.MethodGet, "/api/sessions/local/demo/preview?lines=8&session_name=demo&window_id=@1&window_index=2&pane_id=%253&pane_index=4", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		server.requireRole("control", server.handleSessionAction)(rec, req)
		close(done)
	}()

	var requestID string
	select {
	case env := <-worker.send:
		if env.Type != protocol.TypeSessionPreview {
			t.Fatalf("unexpected type: %s", env.Type)
		}
		requestID = env.ID
		var payload protocol.SessionPreviewRequest
		if err := env.DecodePayload(&payload); err != nil {
			t.Fatal(err)
		}
		if payload.Lines != 8 {
			t.Fatalf("unexpected lines: %d", payload.Lines)
		}
		if payload.Scope != "pane" || payload.Target == nil || payload.Target.PaneID != "%3" || payload.Target.WindowID != "@1" || payload.Target.WindowIndex != 2 || payload.Target.PaneIndex != 4 {
			t.Fatalf("unexpected target payload: %+v", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not receive preview request")
	}

	reply, err := protocol.NewEnvelope(protocol.TypeSessionPreview, protocol.SessionPreview{Data: "pane preview", Scope: "pane"})
	if err != nil {
		t.Fatal(err)
	}
	reply.ID = requestID
	server.handleWorkerMessage(worker, reply)

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("preview request did not complete")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"scope":"pane"`) {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}
}

func TestSessionTargetsRequestForwardsToWorker(t *testing.T) {
	server := New(":0", "secret", nil)
	worker := &workerConn{
		id:   "local",
		send: make(chan protocol.Envelope, 1),
	}
	server.registerWorker(worker)

	req := httptest.NewRequest(http.MethodGet, "/api/sessions/local/demo/targets", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		server.requireRole("control", server.handleSessionAction)(rec, req)
		close(done)
	}()

	var requestID string
	select {
	case env := <-worker.send:
		if env.Type != protocol.TypeSessionTargets {
			t.Fatalf("unexpected type: %s", env.Type)
		}
		requestID = env.ID
		if env.SessionID != "local/demo" {
			t.Fatalf("unexpected session id: %s", env.SessionID)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not receive targets request")
	}

	reply, err := protocol.NewEnvelope(protocol.TypeSessionTargets, protocol.SessionTargets{Targets: []protocol.TerminalTarget{{SessionName: "demo", WindowName: "main", PaneID: "%1"}}})
	if err != nil {
		t.Fatal(err)
	}
	reply.ID = requestID
	server.handleWorkerMessage(worker, reply)

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("targets request did not complete")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"pane_id":"%1"`) {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}
}

func TestSessionActionErrorStatus(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "nil", err: nil, want: http.StatusOK},
		{name: "client canceled", err: context.Canceled, want: http.StatusRequestTimeout},
		{name: "deadline", err: context.DeadlineExceeded, want: http.StatusGatewayTimeout},
		{name: "worker rpc timeout", err: errors.New("session preview timed out"), want: http.StatusGatewayTimeout},
		{name: "worker disconnected", err: errors.New("worker not connected: local"), want: http.StatusServiceUnavailable},
		{name: "worker queue full", err: errors.New("worker send queue full: local"), want: http.StatusServiceUnavailable},
		{name: "worker capture error", err: errors.New("capture failed"), want: http.StatusBadGateway},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := sessionActionErrorStatus(test.err); got != test.want {
				t.Fatalf("status = %d, want %d", got, test.want)
			}
		})
	}
}
