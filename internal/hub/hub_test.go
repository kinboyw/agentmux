package hub

import (
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
	if !strings.Contains(body, "http://agentmux.test") {
		t.Fatalf("base URL missing: %s", body)
	}
}

func TestJoinTokenIncludesControlURL(t *testing.T) {
	server := New(":0", "", nil)
	req := httptest.NewRequest(http.MethodPost, "/api/join-tokens", nil)
	req.Host = "agentmux.test"
	rec := httptest.NewRecorder()

	server.handleJoinTokens(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("unexpected status: %d", rec.Code)
	}
	var payload map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	controlURL, ok := payload["control_url"].(string)
	if !ok || !strings.HasPrefix(controlURL, "http://agentmux.test/control?token=amx_join_") {
		t.Fatalf("unexpected control_url: %#v", payload["control_url"])
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
