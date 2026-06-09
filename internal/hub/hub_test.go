package hub

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	minted, err := server.auth.MintSignal(time.Minute, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	req.Header.Set("Authorization", "Bearer "+minted.Signal)
	if server.authorized(req) {
		t.Fatal("raw signal should not authorize normal APIs")
	}
	exchanged, err := server.auth.Exchange(exchangeRequest{Signal: minted.Signal, Role: "control"})
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	req.Header.Set("Authorization", "Bearer "+exchanged.Credential)
	if !server.authorized(req) {
		t.Fatal("exchanged credential should authorize")
	}
	if !server.authorizedRole(req, "control") {
		t.Fatal("control credential should authorize control routes")
	}
	if server.authorizedRole(req, "worker") {
		t.Fatal("control credential should not authorize worker routes")
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

func TestSignalIncludesControlURL(t *testing.T) {
	server := New(":0", "", nil)
	req := httptest.NewRequest(http.MethodPost, "/api/signals", nil)
	req.Host = "agentmux.test"
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
	if !ok || !strings.HasPrefix(controlURL, "http://agentmux.test/control?signal=amx_sig_") {
		t.Fatalf("unexpected control_url: %#v", payload["control_url"])
	}
	signal, ok := payload["signal"].(string)
	if !ok || !strings.HasPrefix(signal, "amx_sig_") {
		t.Fatalf("unexpected signal: %#v", payload["signal"])
	}
	if got := payload["worker_command"].(string); !strings.Contains(got, "curl -fsSL http://agentmux.test/install.sh") || strings.Contains(got, "go run") {
		t.Fatalf("unexpected worker command: %s", got)
	}
	if got := payload["control_command"].(string); !strings.Contains(got, "curl -fsSL http://agentmux.test/install.sh") || strings.Contains(got, "go run") {
		t.Fatalf("unexpected control command: %s", got)
	}
}

func TestSignalUsesConfiguredPublicURL(t *testing.T) {
	server, err := NewWithOptions(ServerOptions{Addr: ":0", PublicURL: "https://mux.example.com/"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/signals", nil)
	req.Host = "internal.local"
	rec := httptest.NewRecorder()

	server.handleSignals(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("unexpected status: %d", rec.Code)
	}
	var payload map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if got := payload["control_url"].(string); !strings.HasPrefix(got, "https://mux.example.com/control?signal=amx_sig_") {
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

func TestSignalExchangeCredentialAuthorizesAPI(t *testing.T) {
	server := New(":0", "", nil)
	signalReq := httptest.NewRequest(http.MethodPost, "/api/signals", nil)
	signalReq.Host = "agentmux.test"
	signalRec := httptest.NewRecorder()
	server.handleSignals(signalRec, signalReq)
	if signalRec.Code != http.StatusCreated {
		t.Fatalf("unexpected signal status: %d", signalRec.Code)
	}
	var signalPayload map[string]any
	if err := json.NewDecoder(signalRec.Body).Decode(&signalPayload); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"signal":"` + signalPayload["signal"].(string) + `","role":"control","device_name":"test"}`)
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
	apiReq := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	apiReq.Header.Set("Authorization", "Bearer "+exchangePayload["credential"].(string))
	apiRec := httptest.NewRecorder()
	server.requireRole("control", server.handleSessions)(apiRec, apiReq)
	if apiRec.Code != http.StatusOK {
		t.Fatalf("credential did not authorize api: %d body=%s", apiRec.Code, apiRec.Body.String())
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
	mintedA, err := server.auth.MintSignal(time.Minute, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	workerCredA, err := server.auth.Exchange(exchangeRequest{Signal: mintedA.Signal, Role: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	controlCredA, err := server.auth.Exchange(exchangeRequest{Signal: mintedA.Signal, Role: "control"})
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
	server.workers["a"] = &workerConn{id: "a", tenantID: workerCredA.TenantID, send: make(chan protocol.Envelope, 1)}
	server.workers["b"] = &workerConn{id: "b", tenantID: workerCredB.TenantID, send: make(chan protocol.Envelope, 1)}

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

func TestControlOpenForwardsInitialTerminalSize(t *testing.T) {
	server := New(":0", "secret", nil)
	worker := &workerConn{
		id:   "local",
		send: make(chan protocol.Envelope, 1),
	}
	server.workers[worker.id] = worker
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

func TestSessionPreviewRequestForwardsToWorker(t *testing.T) {
	server := New(":0", "secret", nil)
	worker := &workerConn{
		id:   "local",
		send: make(chan protocol.Envelope, 1),
	}
	server.workers[worker.id] = worker

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
