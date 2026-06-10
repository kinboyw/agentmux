package control

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"private/agentmux/internal/protocol"
	"private/agentmux/internal/terminalview"
)

func TestDebugSnapshotOmitsCredentialAndTracksStreamState(t *testing.T) {
	app := NewApp(Client{HubURL: "http://hub?token=query-secret", Token: "secret-token"}, AppAuthResult{
		Source: "test", TenantID: "tenant-1",
	}, nil, nil)
	app.debug.enabled = true
	app.sessions = []protocol.SessionView{{ID: "local/a"}}
	app.streams["local/a"] = &appSessionStream{
		sessionID: "local/a",
		pending:   []byte("abc"),
		size:      protocol.TerminalSize{Cols: 80, Rows: 20},
		warming:   true,
	}
	app.recordDebugKey("down", false)
	app.recordDebugStreamEvent(appStreamEvent{sessionID: "local/a", data: []byte("xyz")})
	app.recordDebugPendingProcessed("local/a", 2, 1)

	raw := app.debugSnapshot("test")
	if strings.Contains(string(raw), "secret-token") || strings.Contains(string(raw), "query-secret") {
		t.Fatalf("debug snapshot leaked credential: %s", string(raw))
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["reason"] != "test" || payload["selected_session"] != "local/a" {
		t.Fatalf("unexpected snapshot metadata: %v", payload)
	}
	if payload["hub"] != "http://hub" {
		t.Fatalf("expected sanitized hub URL, got %v", payload["hub"])
	}
	if payload["stream_bytes"].(float64) != 3 || payload["total_pending"].(float64) != 3 {
		t.Fatalf("unexpected stream counters: %v", payload)
	}
	streams, ok := payload["streams"].([]any)
	if !ok || len(streams) != 1 {
		t.Fatalf("expected one stream snapshot: %v", payload["streams"])
	}
	stream := streams[0].(map[string]any)
	if stream["session_id"] != "local/a" || stream["pending_bytes"].(float64) != 3 {
		t.Fatalf("unexpected stream snapshot: %v", stream)
	}
}

func TestDebugLogWritesManualSnapshot(t *testing.T) {
	path := t.TempDir() + "/tui-debug.log"
	app := NewApp(Client{HubURL: "http://hub?token=query-secret", Token: "secret-token"}, AppAuthResult{}, nil, nil)
	if err := app.EnableDebug(AppDebugOptions{Enabled: true, LogPath: path}); err != nil {
		t.Fatal(err)
	}
	app.recordDebugKey("p", true)
	app.recordDebugKey("enter", true)
	app.sessions = []protocol.SessionView{{ID: "local/a"}}
	app.recordDebugRender(120, 24)
	got, err := app.writeDebugSnapshot("manual")
	if err != nil {
		t.Fatal(err)
	}
	app.closeDebug()
	if got != path {
		t.Fatalf("unexpected debug path: %q", got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "debug enabled") || !strings.Contains(text, "SNAPSHOT") {
		t.Fatalf("debug log missing entries:\n%s", text)
	}
	if strings.Contains(text, "secret-token") || strings.Contains(text, "query-secret") {
		t.Fatalf("debug log leaked credential:\n%s", text)
	}
	if strings.Contains(text, `key "p"`) {
		t.Fatalf("debug log leaked active session input:\n%s", text)
	}
	if !strings.Contains(text, `key "input"`) || !strings.Contains(text, `key "enter"`) {
		t.Fatalf("debug log did not record sanitized key labels:\n%s", text)
	}
}

func TestDebugRenderIncludesHUDAndShortcut(t *testing.T) {
	app := NewApp(Client{HubURL: "http://hub", Token: "token"}, AppAuthResult{}, nil, &strings.Builder{})
	app.debug.enabled = true
	app.status = "ready"
	app.recordDebugKey("down", false)
	app.renderWithSize(120, 24)
	output := stripControl(app.Out.(*strings.Builder).String())
	if !strings.Contains(output, "debug") || !strings.Contains(output, "renders=1") {
		t.Fatalf("debug HUD missing:\n%s", output)
	}
	if !strings.Contains(output, "D debug") {
		t.Fatalf("debug shortcut missing:\n%s", output)
	}
}

func TestDebugActiveRenderKeepsTerminalCanvasClean(t *testing.T) {
	app := NewApp(Client{HubURL: "http://hub", Token: "token"}, AppAuthResult{}, nil, &strings.Builder{})
	app.debug.enabled = true
	app.status = "attached"
	app.active = "local/a"
	app.fullscreen = true
	view := terminalview.New(20, 5)
	view.Write([]byte("remote"))
	app.streams["local/a"] = &appSessionStream{
		sessionID:     "local/a",
		view:          view,
		seenOutput:    true,
		visibleOutput: true,
	}
	app.renderWithSize(120, 24)
	output := stripControl(app.Out.(*strings.Builder).String())
	if strings.Contains(output, "Ctrl-G debug") || strings.Contains(output, "debug") {
		t.Fatalf("active terminal canvas should not include local debug chrome:\n%s", output)
	}
}

func TestDebugActiveFallbackShowsDebugShortcut(t *testing.T) {
	app := NewApp(Client{HubURL: "http://hub", Token: "token"}, AppAuthResult{}, nil, &strings.Builder{})
	app.debug.enabled = true
	app.active = "local/a"
	app.fullscreen = true
	app.renderWithSize(120, 24)
	output := stripControl(app.Out.(*strings.Builder).String())
	if !strings.Contains(output, "Ctrl-G debug") {
		t.Fatalf("active fallback should include debug shortcut:\n%s", output)
	}
}

func TestDebugCtrlGWritesSnapshotWhileAttached(t *testing.T) {
	path := t.TempDir() + "/tui-debug.log"
	app := NewApp(Client{HubURL: "http://hub", Token: "token"}, AppAuthResult{}, nil, nil)
	if err := app.EnableDebug(AppDebugOptions{Enabled: true, LogPath: path}); err != nil {
		t.Fatal(err)
	}
	app.active = "local/a"
	app.handleKey(t.Context(), "ctrl-g")
	app.closeDebug()
	if !strings.Contains(app.status, "debug snapshot") {
		t.Fatalf("expected debug snapshot status, got %q", app.status)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "SNAPSHOT") {
		t.Fatalf("debug log missing snapshot:\n%s", string(data))
	}
}
