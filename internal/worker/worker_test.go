package worker

import (
	"context"
	"io"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	"private/agentmux/internal/credentialcache"
	"private/agentmux/internal/protocol"
	"private/agentmux/internal/sessionbackend"
	"private/agentmux/internal/terminalview"
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

func TestDefaultWorkerSoftwareInventory(t *testing.T) {
	t.Setenv("AGENTMUX_WORKER_SERVICE_BACKEND", "systemd-user")
	software := defaultWorkerSoftware()
	if software.ProtocolVersion != protocol.ProtocolVersion {
		t.Fatalf("unexpected protocol version: %+v", software)
	}
	if software.OS != runtime.GOOS || software.Arch != runtime.GOARCH || software.ServiceBackend != "systemd-user" {
		t.Fatalf("unexpected software inventory: %+v", software)
	}
	if len(software.Capabilities) == 0 {
		t.Fatal("expected worker capabilities")
	}
}

func TestTerminalOpenRequestsCapability(t *testing.T) {
	open := protocol.TerminalOpen{Capabilities: []string{"terminal.snapshot.v1"}}
	if !terminalOpenRequestsCapability(open, "terminal.snapshot.v1") {
		t.Fatal("expected snapshot capability to be detected")
	}
	if terminalOpenRequestsCapability(open, "terminal.diff.v1") {
		t.Fatal("unexpected diff capability")
	}
}

func TestEffectiveTerminalModeRequiresExplicitStateRequest(t *testing.T) {
	w := New("https://hub.test", "token", "worker", "worker", nilBackend{}, nil)
	w.TerminalMode = "auto"
	if got := w.effectiveTerminalMode(protocol.TerminalOpen{}); got != "attach" {
		t.Fatalf("legacy open should use attach, got %q", got)
	}
	got := w.effectiveTerminalMode(protocol.TerminalOpen{
		TransportMode: "auto",
		Capabilities:  []string{"terminal.snapshot.v1"},
	})
	if got != "state-bridge" {
		t.Fatalf("state-capable open should use state bridge, got %q", got)
	}
	w.TerminalMode = "attach"
	got = w.effectiveTerminalMode(protocol.TerminalOpen{
		TransportMode: "state",
		Capabilities:  []string{"terminal.snapshot.v1"},
	})
	if got != "attach" {
		t.Fatalf("worker attach config should force attach, got %q", got)
	}
}

func TestEffectiveRenderModeNegotiatesWorkerStateXterm(t *testing.T) {
	w := New("https://hub.test", "token", "worker", "worker", nilBackend{}, nil)
	open := protocol.TerminalOpen{
		RenderMode:   protocol.RenderModeWorkerStateXterm,
		Capabilities: []string{"terminal.snapshot.v1"},
	}
	mode := w.effectiveTerminalMode(open)
	if mode != "state-bridge" {
		t.Fatalf("expected state bridge, got %q", mode)
	}
	if got := w.effectiveRenderMode(open, mode); got != protocol.RenderModeWorkerStateXterm {
		t.Fatalf("unexpected render mode: %q", got)
	}
	capabilities := w.terminalModeCapabilities(mode, protocol.RenderModeWorkerStateXterm, false)
	if containsString(capabilities, "terminal.cells.v1") {
		t.Fatalf("cells should be opt-in, got %+v", capabilities)
	}
	if !containsString(capabilities, "terminal.render.worker_state_xterm.v1") {
		t.Fatalf("worker_state_xterm capability missing: %+v", capabilities)
	}
}

func TestTerminalHistoryPageUsesLimitAndBeforeSeq(t *testing.T) {
	w := New("https://hub.test", "token", "worker", "worker", nilBackend{}, nil)
	sessionID := protocol.SessionID("worker", "demo")
	w.recordTerminalOutput(sessionID, []byte("one\ntwo\nthree\n"))

	page := w.historyPage(sessionID, protocol.TerminalHistoryRequest{LimitLines: 2})
	if len(page.Lines) != 2 {
		t.Fatalf("expected 2 lines, got %+v", page)
	}
	if page.Lines[0].Text != "two" || page.Lines[1].Text != "three" {
		t.Fatalf("unexpected page lines: %+v", page.Lines)
	}
	if !page.HasMore || page.StartSeq == 0 || page.EndSeq == 0 {
		t.Fatalf("unexpected page metadata: %+v", page)
	}

	older := w.historyPage(sessionID, protocol.TerminalHistoryRequest{BeforeSeq: page.StartSeq, LimitLines: 2})
	if len(older.Lines) != 1 || older.Lines[0].Text != "one" || older.HasMore {
		t.Fatalf("unexpected older page: %+v", older)
	}
}

func TestTerminalHistoryBoundaryRecordsGeneration(t *testing.T) {
	w := New("https://hub.test", "token", "worker", "worker", nilBackend{}, nil)
	sessionID := protocol.SessionID("worker", "demo")
	generation := w.nextGeneration(sessionID)
	w.recordTerminalBoundary(sessionID, generation, "--- size_sync: 100x30 ---", "resize_boundary")

	page := w.historyPage(sessionID, protocol.TerminalHistoryRequest{})
	if len(page.Lines) != 1 {
		t.Fatalf("expected one history line, got %+v", page)
	}
	line := page.Lines[0]
	if line.Generation != generation || line.Text == "" || len(line.Flags) != 1 || line.Flags[0] != "resize_boundary" {
		t.Fatalf("unexpected boundary line: %+v", line)
	}
}

func TestTerminalStateKeyIncludesPaneTarget(t *testing.T) {
	sessionID := protocol.SessionID("worker", "demo")
	paneA := &protocol.TerminalTarget{SessionName: "demo", WindowID: "@1", PaneID: "%1"}
	paneB := &protocol.TerminalTarget{SessionName: "demo", WindowID: "@1", PaneID: "%2"}

	keyA := terminalStateKey(sessionID, paneA)
	keyB := terminalStateKey(sessionID, paneB)
	if keyA == keyB || !strings.Contains(keyA, "pane_id=%1") || !strings.Contains(keyB, "pane_id=%2") {
		t.Fatalf("expected pane-specific state keys, got %q and %q", keyA, keyB)
	}
	if !terminalStateKeyMatchesSession(keyA, sessionID) || terminalStateKeyMatchesSession(keyA, "worker/other") {
		t.Fatalf("unexpected state key session match for %q", keyA)
	}
}

func TestTerminalHistoryUsesTargetScopedStateKey(t *testing.T) {
	w := New("https://hub.test", "token", "worker", "worker", nilBackend{}, nil)
	sessionID := protocol.SessionID("worker", "demo")
	paneA := terminalStateKey(sessionID, &protocol.TerminalTarget{SessionName: "demo", WindowID: "@1", PaneID: "%1"})
	paneB := terminalStateKey(sessionID, &protocol.TerminalTarget{SessionName: "demo", WindowID: "@1", PaneID: "%2"})

	w.recordTerminalOutput(paneA, []byte("pane-a\n"))
	w.recordTerminalOutput(paneB, []byte("pane-b\n"))

	pageA := w.historyPage(paneA, protocol.TerminalHistoryRequest{})
	pageB := w.historyPage(paneB, protocol.TerminalHistoryRequest{})
	if len(pageA.Lines) != 1 || pageA.Lines[0].Text != "pane-a" {
		t.Fatalf("unexpected pane A history: %+v", pageA)
	}
	if len(pageB.Lines) != 1 || pageB.Lines[0].Text != "pane-b" {
		t.Fatalf("unexpected pane B history: %+v", pageB)
	}
}

func TestTerminalStateStreamProducesCellSnapshot(t *testing.T) {
	w := New("https://hub.test", "token", "worker", "worker", nilBackend{}, nil)
	w.streamStates["stream-1"] = &terminalStateStream{
		view: terminalview.New(10, 2),
		size: protocol.TerminalSize{Cols: 10, Rows: 2},
	}
	w.recordTerminalState("stream-1", []byte("abc\rZ"))
	cells, ok := w.streamCellSnapshot("stream-1")
	if !ok {
		t.Fatal("expected stream cell snapshot")
	}
	if cells.Version != "cells-v1" || cells.Cols != 10 || cells.Rows != 2 {
		t.Fatalf("unexpected cells metadata: %+v", cells)
	}
	if got := cells.Lines[0][0].Text + cells.Lines[0][1].Text + cells.Lines[0][2].Text; got != "Zbc" {
		t.Fatalf("unexpected cells content: %q", got)
	}
}

func TestTerminalStateStreamProducesColoredCells(t *testing.T) {
	w := New("https://hub.test", "token", "worker", "worker", nilBackend{}, nil)
	w.streamStates["stream-1"] = &terminalStateStream{
		view: terminalview.New(10, 2),
		size: protocol.TerminalSize{Cols: 10, Rows: 2},
	}
	w.recordTerminalState("stream-1", []byte("\x1b[31mR\x1b[0m \x1b[48;5;33mB"))
	cells, ok := w.streamCellSnapshot("stream-1")
	if !ok {
		t.Fatal("expected stream cell snapshot")
	}
	if cells.Lines[0][0].Fg != "ansi:1" {
		t.Fatalf("expected fg color to be preserved, got %+v", cells.Lines[0][0])
	}
	if cells.Lines[0][2].Bg != "ansi:33" {
		t.Fatalf("expected bg color to be preserved, got %+v", cells.Lines[0][2])
	}
}

func TestTerminalStateANSISnapshotUsesTargetState(t *testing.T) {
	w := New("https://hub.test", "token", "worker", "worker", nilBackend{}, nil)
	sessionID := protocol.SessionID("worker", "demo")
	target := &protocol.TerminalTarget{SessionName: "demo", WindowID: "@1", PaneID: "%1"}
	key := terminalStateKey(sessionID, target)
	view := terminalview.New(10, 2)
	view.Write([]byte("\x1b[31mred"))
	w.streamStates[key] = &terminalStateStream{
		view: view,
		size: protocol.TerminalSize{Cols: 10, Rows: 2},
	}

	data, cells, ok := w.stateANSIForTarget(sessionID, target)
	if !ok {
		t.Fatal("expected target state ANSI snapshot")
	}
	if cells.Cols != 10 || cells.Rows != 2 || !strings.Contains(data, "\x1b[0;31mred") {
		t.Fatalf("unexpected ANSI state snapshot cols=%d rows=%d data=%q", cells.Cols, cells.Rows, data)
	}
	if _, _, ok := w.stateANSIForTarget(sessionID, &protocol.TerminalTarget{SessionName: "demo", WindowID: "@1", PaneID: "%2"}); ok {
		t.Fatal("unexpected snapshot for another pane")
	}
}

func TestTerminalCellDiffReplacesChangedRowsAndCursor(t *testing.T) {
	previous := protocol.TerminalCellSnapshot{
		Cols: 3,
		Rows: 2,
		Cursor: protocol.TerminalCursor{
			X: 0, Y: 0, Visible: true,
		},
		Lines: [][]protocol.TerminalCell{
			{{Text: "a"}, {Text: "b"}, {Text: "c"}},
			{{Text: "1"}, {Text: "2"}, {Text: "3"}},
		},
	}
	current := protocol.TerminalCellSnapshot{
		Cols: 3,
		Rows: 2,
		Cursor: protocol.TerminalCursor{
			X: 2, Y: 1, Visible: true,
		},
		Lines: [][]protocol.TerminalCell{
			{{Text: "a"}, {Text: "b"}, {Text: "c"}},
			{{Text: "1"}, {Text: "9", Fg: "ansi:1"}, {Text: "3"}},
		},
	}

	diff, ok := terminalCellDiff(7, previous, current)
	if !ok {
		t.Fatal("expected diff to be produced")
	}
	if diff.Generation != 7 || len(diff.Ops) != 2 {
		t.Fatalf("unexpected diff: %+v", diff)
	}
	if diff.Ops[0].Op != "replace_row" || diff.Ops[0].Row != 1 || diff.Ops[0].Cells[1].Text != "9" || diff.Ops[0].Cells[1].Fg != "ansi:1" {
		t.Fatalf("unexpected row diff: %+v", diff.Ops[0])
	}
	if diff.Ops[1].Op != "cursor" || diff.Ops[1].Cursor == nil || diff.Ops[1].Cursor.X != 2 || diff.Ops[1].Cursor.Y != 1 {
		t.Fatalf("unexpected cursor diff: %+v", diff.Ops[1])
	}
}

func TestTerminalMouseOnlyWritesWhenRemoteMouseModeEnabled(t *testing.T) {
	stream := &recordingStream{}
	w := New("https://hub.test", "token", "worker", "worker", nilBackend{}, nil)
	w.terms["stream-1"] = stream
	view := terminalview.New(10, 2)
	w.streamStates["stream-1"] = &terminalStateStream{
		view: view,
		size: protocol.TerminalSize{Cols: 10, Rows: 2},
	}

	if w.writeTerminalMouse("stream-1", protocol.TerminalMouse{X: 1, Y: 1, Button: "left"}) {
		t.Fatal("mouse input should be ignored until remote enables mouse mode")
	}
	if stream.writes != "" {
		t.Fatalf("unexpected stream writes: %q", stream.writes)
	}

	view.Write([]byte("\x1b[?1000h\x1b[?1006h"))
	if !w.writeTerminalMouse("stream-1", protocol.TerminalMouse{X: 1, Y: 1, Button: "left"}) {
		t.Fatal("expected mouse input to be written after mouse mode is enabled")
	}
	if stream.writes != "\x1b[<0;2;2M" {
		t.Fatalf("unexpected mouse sequence: %q", stream.writes)
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
			Body:       io.NopCloser(strings.NewReader(`{"credential":"amx_cred_worker","credential_id":"cred_worker","tenant_id":"tenant_test","role":"worker","device_id":"dev_server","expires_at":"` + expires.Format(time.RFC3339Nano) + `","refresh_token":"amx_ref_worker","refresh_expires_at":"` + expires.Add(time.Hour).Format(time.RFC3339Nano) + `","scopes":["worker"]}`)),
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
	if cached.Credential != "amx_cred_worker" || cached.CredentialID != "cred_worker" || cached.RefreshToken != "amx_ref_worker" {
		t.Fatalf("unexpected cached credential: %+v", cached)
	}
}

func TestWorkerEnsureFreshCredentialRefreshesCachedWorker(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	expires := time.Now().UTC().Add(time.Hour)
	oldClient := http.DefaultClient
	t.Cleanup(func() { http.DefaultClient = oldClient })
	http.DefaultClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/auth/refresh" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		return &http.Response{
			StatusCode: http.StatusCreated,
			Status:     "201 Created",
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"credential":"amx_cred_new","credential_id":"cred_new","tenant_id":"tenant_test","role":"worker","device_id":"dev_server","expires_at":"` + expires.Format(time.RFC3339Nano) + `","refresh_token":"amx_ref_new","refresh_expires_at":"` + expires.Add(time.Hour).Format(time.RFC3339Nano) + `","scopes":["worker"]}`)),
			Request:    r,
		}, nil
	})}

	w := New("https://hub.test", "amx_cred_old", "dev_server", "server", nilBackend{}, nil)
	w.WithCredentialEntry(credentialcache.Entry{
		HubURL: "https://hub.test", Credential: "amx_cred_old", CredentialID: "cred_old",
		TenantID: "tenant_test", Role: "worker", DeviceID: "dev_server", DeviceName: "server",
		ExpiresAt: time.Now().UTC().Add(time.Minute), RefreshToken: "amx_ref_old",
		RefreshExpiresAt: time.Now().UTC().Add(time.Hour), UpdatedAt: time.Now().UTC(),
	})
	if err := w.EnsureFreshCredential(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := w.currentToken(); got != "amx_cred_new" {
		t.Fatalf("expected refreshed current token, got %q", got)
	}
	cached, ok := credentialcache.Load("https://hub.test", "worker", "dev_server")
	if !ok {
		t.Fatal("expected refreshed credential in cache")
	}
	if cached.Credential != "amx_cred_new" || cached.RefreshToken != "amx_ref_new" {
		t.Fatalf("unexpected refreshed cache entry: %+v", cached)
	}
}

func TestResolveAuthRejectsJoinWhenAlreadyJoinedToAnotherHub(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := credentialcache.Save(credentialcache.Entry{
		HubURL: "https://old-hub.test", Role: "worker", DeviceID: "dev_cached",
		DeviceName: "cached", Credential: "amx_cred_cached",
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	oldClient := http.DefaultClient
	t.Cleanup(func() { http.DefaultClient = oldClient })
	http.DefaultClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Fatalf("join should not call exchange when worker is bound to another hub")
		return nil, nil
	})}

	_, err := ResolveAuth(context.Background(), AuthOptions{
		HubURL: "https://new-hub.test", Join: "amx_sig_test", DeviceID: "dev_new", DeviceName: "renamed",
	})
	if err == nil || !strings.Contains(err.Error(), "already joined") {
		t.Fatalf("expected already joined error, got %v", err)
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

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

type nilBackend struct{}

func (nilBackend) Name() string                                            { return "nil" }
func (nilBackend) List(context.Context) ([]sessionbackend.Session, error)  { return nil, nil }
func (nilBackend) Create(context.Context, string, string, string) error    { return nil }
func (nilBackend) Kill(context.Context, string) error                      { return nil }
func (nilBackend) SendTerminalInput(context.Context, string, string) error { return nil }
func (nilBackend) Capture(context.Context, string, int) (string, error)    { return "", nil }
func (nilBackend) Open(context.Context, string, int, int) (sessionbackend.Stream, error) {
	return nil, nil
}

type recordingStream struct {
	writes string
}

func (s *recordingStream) Read([]byte) (int, error) { return 0, io.EOF }
func (s *recordingStream) Resize(int, int) error    { return nil }
func (s *recordingStream) Close() error             { return nil }
func (s *recordingStream) Write(data []byte) (int, error) {
	s.writes += string(data)
	return len(data), nil
}
