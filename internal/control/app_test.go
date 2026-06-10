package control

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	"private/agentmux/internal/protocol"
	"private/agentmux/internal/terminalview"
)

func TestDedupeSessionsSortsByID(t *testing.T) {
	sessions := dedupeSessions([]protocol.SessionView{
		{ID: "worker/z", Status: "old"},
		{ID: "worker/a", Status: "active"},
		{ID: "worker/z", Status: "active"},
	})
	if len(sessions) != 2 {
		t.Fatalf("expected 2 sessions, got %d: %+v", len(sessions), sessions)
	}
	if sessions[0].ID != "worker/a" || sessions[1].ID != "worker/z" {
		t.Fatalf("sessions not sorted: %+v", sessions)
	}
	if sessions[1].Status != "active" {
		t.Fatalf("expected later duplicate to win: %+v", sessions[1])
	}
}

func TestDedupeWorkersKeepsNewestLastSeen(t *testing.T) {
	oldTime := time.Now().UTC().Add(-time.Hour)
	newTime := time.Now().UTC()
	workers := dedupeWorkers([]protocol.WorkerView{
		{ID: "local", Name: "old", LastSeen: oldTime},
		{ID: "local", Name: "new", LastSeen: newTime},
	})
	if len(workers) != 1 {
		t.Fatalf("expected 1 worker, got %d", len(workers))
	}
	if workers[0].Name != "new" {
		t.Fatalf("expected newest worker: %+v", workers[0])
	}
}

func TestDefaultWorkerIDPrefersOnlineWorker(t *testing.T) {
	workers := []protocol.WorkerView{
		{ID: "offline", Status: "offline", Online: false},
		{ID: "online", Status: "online", Online: true},
	}
	if got := defaultWorkerID(workers); got != "online" {
		t.Fatalf("expected online worker, got %q", got)
	}
}

func TestReadAppKeyEscDoesNotQuit(t *testing.T) {
	key, err := readAppKey(strings.NewReader("\x1b"))
	if err != nil {
		t.Fatal(err)
	}
	if key != "unknown" {
		t.Fatalf("expected bare esc to be ignored, got %q", key)
	}
}

func TestReadAppKeyArrows(t *testing.T) {
	key, err := readAppKey(strings.NewReader("\x1b[A"))
	if err != nil {
		t.Fatal(err)
	}
	if key != "up" {
		t.Fatalf("expected up, got %q", key)
	}
	key, err = readAppKey(strings.NewReader("\x1b[B"))
	if err != nil {
		t.Fatal(err)
	}
	if key != "down" {
		t.Fatalf("expected down, got %q", key)
	}
	key, err = readAppKey(strings.NewReader("\x1bOB"))
	if err != nil {
		t.Fatal(err)
	}
	if key != "down" {
		t.Fatalf("expected application cursor down, got %q", key)
	}
}

func TestReadAppKeyDetach(t *testing.T) {
	key, err := readAppKey(strings.NewReader("\x1d"))
	if err != nil {
		t.Fatal(err)
	}
	if key != "detach" {
		t.Fatalf("expected detach, got %q", key)
	}
}

func TestReadAppKeyCtrlGDebugSnapshot(t *testing.T) {
	key, err := readAppKey(strings.NewReader("\x07"))
	if err != nil {
		t.Fatal(err)
	}
	if key != "ctrl-g" {
		t.Fatalf("expected ctrl-g, got %q", key)
	}
}

func TestReadAppKeyCtrlQHardQuit(t *testing.T) {
	key, err := readAppKey(strings.NewReader("\x11"))
	if err != nil {
		t.Fatal(err)
	}
	if key != "ctrl-q" {
		t.Fatalf("expected ctrl-q, got %q", key)
	}
}

func TestReadAppKeyCtrlFTogglesFullscreen(t *testing.T) {
	key, err := readAppKey(strings.NewReader("\x06"))
	if err != nil {
		t.Fatal(err)
	}
	if key != "ctrl-f" {
		t.Fatalf("expected ctrl-f, got %q", key)
	}
}

func TestAppKeyReaderPreservesUTF8InputBytes(t *testing.T) {
	input := "中文"
	reader := strings.NewReader(input)
	var keys appKeyReader
	var got strings.Builder
	for range input {
		key, err := keys.Read(reader)
		if err != nil {
			t.Fatal(err)
		}
		got.WriteString(key)
	}
	if got.String() != input {
		t.Fatalf("utf-8 input changed: got %q bytes=% x want %q bytes=% x", got.String(), []byte(got.String()), input, []byte(input))
	}
}

func TestAppKeyReaderParsesSGRMouseClick(t *testing.T) {
	var keys appKeyReader
	input, ok, err := keys.ReadEventAvailable(strings.NewReader("\x1b[<0;12;5M"))
	if err != nil {
		t.Fatal(err)
	}
	if !ok || input.mouse == nil {
		t.Fatalf("expected mouse event, got ok=%t input=%+v", ok, input)
	}
	mouse := *input.mouse
	if mouse.kind != appMouseClick || mouse.x != 12 || mouse.y != 5 || mouse.button != terminalview.MouseLeft {
		t.Fatalf("unexpected mouse event: %+v", mouse)
	}
}

func TestAppKeyReaderParsesSGRMouseRelease(t *testing.T) {
	var keys appKeyReader
	input, ok, err := keys.ReadEventAvailable(strings.NewReader("\x1b[<0;12;5m"))
	if err != nil {
		t.Fatal(err)
	}
	if !ok || input.mouse == nil {
		t.Fatalf("expected mouse event, got ok=%t input=%+v", ok, input)
	}
	if input.mouse.kind != appMouseRelease || input.mouse.x != 12 || input.mouse.y != 5 {
		t.Fatalf("unexpected mouse release: %+v", *input.mouse)
	}
}

func TestAppKeyInputDataAcceptsUTF8Rune(t *testing.T) {
	input := "中"
	if got := appKeyInputData(input); got != input {
		t.Fatalf("utf-8 rune input changed: got %q bytes=% x want %q bytes=% x", got, []byte(got), input, []byte(input))
	}
}

func TestAppKeyReaderHandlesSplitArrowSequence(t *testing.T) {
	reader := &chunkReader{chunks: [][]byte{
		[]byte("\x1b"),
		[]byte("["),
		[]byte("B"),
	}}
	var keys appKeyReader
	key, err := keys.Read(reader)
	if err != nil {
		t.Fatal(err)
	}
	if key != "down" {
		t.Fatalf("expected split down arrow, got %q", key)
	}
}

func TestVisibleLenASCIIStyledIcons(t *testing.T) {
	line := styleAccent("> ") + "local/demo " + styleOK("* active")
	if got, want := visibleLen(line), len("> local/demo * active"); got != want {
		t.Fatalf("unexpected visible len: got %d want %d", got, want)
	}
}

func TestAppKeyReaderResetClearsPendingBytes(t *testing.T) {
	var keys appKeyReader
	key, err := keys.Read(strings.NewReader("\x1b["))
	if err != nil {
		t.Fatal(err)
	}
	if key != "unknown" {
		t.Fatalf("expected unknown, got %q", key)
	}
	keys.Reset()
	key, err = keys.Read(strings.NewReader("\x1b[B"))
	if err != nil {
		t.Fatal(err)
	}
	if key != "down" {
		t.Fatalf("expected down after reset, got %q", key)
	}
}

func TestSplitPreviewLinesUsesTail(t *testing.T) {
	lines := splitPreviewLines("one\ntwo\nthree\n", 2)
	if strings.Join(lines, ",") != "two,three" {
		t.Fatalf("unexpected preview lines: %+v", lines)
	}
}

func TestStripControlRemovesEscapeSequences(t *testing.T) {
	got := stripControl("\x1b[31mred\x1b[0m\x03")
	if got != "red" {
		t.Fatalf("unexpected stripped line: %q", got)
	}
}

func TestSanitizePreviewANSIKeepsSGR(t *testing.T) {
	got := sanitizePreviewANSI("\x1b[31mred\x1b[0m\x1b[2J")
	if !strings.Contains(got, "\x1b[31mred\x1b[0m") {
		t.Fatalf("expected SGR color to remain: %q", got)
	}
	if strings.Contains(got, "\x1b[2J") {
		t.Fatalf("non-SGR escape should be removed: %q", got)
	}
}

func TestHighlightPreviewLinePreservesExistingSGR(t *testing.T) {
	input := "\x1b[31mred\x1b[0m"
	if got := highlightPreviewLine(input); got != input {
		t.Fatalf("existing SGR should be preserved: %q", got)
	}
}

func TestHighlightPreviewLineAddsFallbackColor(t *testing.T) {
	if got := highlightPreviewLine("error: failed"); !strings.Contains(got, "\x1b[31m") {
		t.Fatalf("expected error highlight: %q", got)
	}
	if got := highlightPreviewLine("ready"); !strings.Contains(got, "\x1b[32m") {
		t.Fatalf("expected ok highlight: %q", got)
	}
	if got := highlightPreviewLine("$ pwd"); !strings.Contains(got, "\x1b[36m") {
		t.Fatalf("expected prompt highlight: %q", got)
	}
}

func TestCompactSessionLineFitsNarrowList(t *testing.T) {
	line := compactSessionLine("> ", protocol.SessionView{
		ID:      "worker/very-long-session-name",
		Status:  "active",
		Command: "very-long-command",
	})
	if visibleLen(line) > 34 {
		t.Fatalf("compact line too wide: %d %q", visibleLen(line), line)
	}
	if !strings.Contains(stripControl(line), "worker/") {
		t.Fatalf("session id disappeared: %q", line)
	}
	if strings.Contains(stripControl(line), "\x1b") {
		t.Fatalf("compact line should not embed raw escapes in visible text: %q", line)
	}
	if !strings.Contains(line, "~") {
		t.Fatalf("expected ellipsis marker: %q", line)
	}
}

func TestAppLayoutUsesNarrowSessionList(t *testing.T) {
	listWidth, previewWidth, limit := appLayout(120, 24)
	if listWidth > 34 {
		t.Fatalf("left list too wide: %d", listWidth)
	}
	if previewWidth <= listWidth {
		t.Fatalf("expected wider preview/session pane: list=%d preview=%d", listWidth, previewWidth)
	}
	if limit != 17 {
		t.Fatalf("unexpected body limit: %d", limit)
	}
}

func TestAppLayoutShowsPreviewAtStandardTerminalWidth(t *testing.T) {
	listWidth, previewWidth, limit := appLayout(80, 24)
	if listWidth != 24 {
		t.Fatalf("unexpected compact list width: %d", listWidth)
	}
	if previewWidth <= 0 {
		t.Fatalf("expected preview at 80 columns")
	}
	if limit != 17 {
		t.Fatalf("unexpected body limit: %d", limit)
	}
}

func TestAppLayoutUsesCustomSplitWidth(t *testing.T) {
	app := NewApp(Client{}, AppAuthResult{}, nil, &bytes.Buffer{})
	app.setSplitWidth(40, 120)
	listWidth, previewWidth, _ := app.layoutForSize(120, 24)
	if listWidth != 40 {
		t.Fatalf("expected custom split width, got %d", listWidth)
	}
	if previewWidth != 77 {
		t.Fatalf("unexpected preview width: %d", previewWidth)
	}
}

func TestPreviewLineCanBeTruncatedToWidth(t *testing.T) {
	line := previewLine([]string{"\x1b[31m" + strings.Repeat("x", 80) + "\x1b[0m"}, 0)
	got := truncateVisible(line, 20)
	if visibleLen(got) > 20 {
		t.Fatalf("preview line too wide: %d", visibleLen(got))
	}
}

func TestVisibleWidthTreatsBoxDrawingAsSingleCells(t *testing.T) {
	line := "├" + strings.Repeat("─", 10) + "┤"
	if got, want := visibleLen(line), 12; got != want {
		t.Fatalf("unexpected box drawing width: got %d want %d", got, want)
	}
	got := stripControl(truncateVisible(line, 6))
	if got != "├─────" {
		t.Fatalf("box drawing truncation split cells: %q", got)
	}
}

func TestRenderIncludesSessionListWhenPreviewExists(t *testing.T) {
	app := &App{
		Out: &bytes.Buffer{},
		sessions: []protocol.SessionView{{
			ID: "local/demo", WorkerID: "local", Name: "demo", Status: "active", Command: "bash",
		}},
		preview: "colored \x1b[31merror\x1b[0m",
		status:  "ready",
	}
	app.renderWithSize(120, 24)
	output := stripControl(app.Out.(*bytes.Buffer).String())
	if !strings.Contains(output, "local/demo") {
		t.Fatalf("session list missing from render:\n%s", output)
	}
	if !strings.Contains(output, "colored error") {
		t.Fatalf("preview missing from render:\n%s", output)
	}
}

func TestRenderDoesNotShowWarmingWithoutSelectedStream(t *testing.T) {
	app := &App{
		Out: &bytes.Buffer{},
		sessions: []protocol.SessionView{{
			ID: "local/demo", WorkerID: "local", Name: "demo", Status: "active", Command: "bash",
		}},
		status: "ready",
	}
	app.renderWithSize(120, 24)
	output := stripControl(app.Out.(*bytes.Buffer).String())
	if strings.Contains(output, "stream warming") {
		t.Fatalf("unexpected warming placeholder without live stream:\n%s", output)
	}
}

func TestRenderShowsWarmingForSelectedWarmingStream(t *testing.T) {
	app := &App{
		Out: &bytes.Buffer{},
		sessions: []protocol.SessionView{{
			ID: "local/demo", WorkerID: "local", Name: "demo", Status: "active", Command: "bash",
		}},
		streams: map[string]*appSessionStream{
			"local/demo": {sessionID: "local/demo", warming: true},
		},
		status: "ready",
	}
	app.renderWithSize(120, 24)
	output := stripControl(app.Out.(*bytes.Buffer).String())
	if !strings.Contains(output, "stream warming") {
		t.Fatalf("expected warming placeholder for warming stream:\n%s", output)
	}
}

func TestRenderUnauthenticatedApp(t *testing.T) {
	app := NewUnauthApp(nil, &bytes.Buffer{}, nil)
	app.status = "not logged in; type /login"
	app.renderWithSize(120, 24)
	output := stripControl(app.Out.(*bytes.Buffer).String())
	if !strings.Contains(output, "auth=none") || !strings.Contains(output, "/login") {
		t.Fatalf("unauthenticated render missing login prompt:\n%s", output)
	}
}

func TestRenderActiveUsesTerminalCanvasOnly(t *testing.T) {
	view := terminalview.New(20, 5)
	view.Write([]byte("\x1b[31mremote\x1b[0m\nshell"))
	app := &App{
		Out:        &bytes.Buffer{},
		active:     "local/demo",
		fullscreen: true,
		sessions:   []protocol.SessionView{{ID: "local/demo"}},
		streams: map[string]*appSessionStream{
			"local/demo": {sessionID: "local/demo", view: view, size: protocol.TerminalSize{Cols: 20, Rows: 5}, seenOutput: true, visibleOutput: true},
		},
		status: "ready",
	}
	app.renderWithSize(20, 5)
	output := stripControl(app.Out.(*bytes.Buffer).String())
	if !strings.Contains(output, "remote") || !strings.Contains(output, "shell") {
		t.Fatalf("active terminal canvas missing remote output:\n%s", output)
	}
	if strings.Contains(output, "Sessions") || strings.Contains(output, "Selected") || strings.Contains(output, "Ctrl-]") {
		t.Fatalf("active render should not include TUI chrome:\n%s", output)
	}
	if got := app.streams["local/demo"].size; got != (protocol.TerminalSize{Cols: 20, Rows: 5}) {
		t.Fatalf("active stream size mismatch: %+v", got)
	}
}

func TestRenderAttachedUsesSplitPaneByDefault(t *testing.T) {
	view := terminalview.New(80, 17)
	view.Write([]byte("remote shell"))
	app := NewApp(Client{HubURL: "http://hub", Token: "token"}, AppAuthResult{}, nil, &bytes.Buffer{})
	app.active = "local/demo"
	app.status = attachedPreviewStatus("local/demo")
	app.sessions = []protocol.SessionView{{ID: "local/demo", Status: "active", Command: "bash"}}
	app.streams["local/demo"] = &appSessionStream{
		sessionID:  "local/demo",
		view:       view,
		size:       protocol.TerminalSize{Cols: 80, Rows: 17},
		writes:     make(chan appStreamWrite, 1),
		seenOutput: true,
	}
	app.updateStreamBuffer(app.streams["local/demo"])
	app.loadSelectedPreview()
	app.renderWithSize(120, 24)
	output := stripControl(app.Out.(*bytes.Buffer).String())
	if !strings.Contains(output, "Sessions") || !strings.Contains(output, "Selected local/demo") {
		t.Fatalf("attached split render should keep local chrome:\n%s", output)
	}
	if !strings.Contains(output, "Session local/demo") || !strings.Contains(output, "remote shell") {
		t.Fatalf("attached split render should show live session pane:\n%s", output)
	}
	if !strings.Contains(output, "Ctrl-F fullscreen") {
		t.Fatalf("attached split render missing fullscreen hint:\n%s", output)
	}
}

func TestRenderActiveUsesPreviewFallbackBeforeFirstStreamOutput(t *testing.T) {
	app := NewApp(Client{HubURL: "http://hub", Token: "token"}, AppAuthResult{}, nil, &bytes.Buffer{})
	app.active = "local/demo"
	app.fullscreen = true
	app.previews["local/demo"] = "cached shell\n$ ready"
	app.streams["local/demo"] = &appSessionStream{
		sessionID: "local/demo",
		view:      terminalview.New(20, 5),
		size:      protocol.TerminalSize{Cols: 20, Rows: 5},
	}
	app.renderWithSize(100, 5)
	output := stripControl(app.Out.(*bytes.Buffer).String())
	if !strings.Contains(output, "cached shell") || !strings.Contains(output, "$ ready") {
		t.Fatalf("active fallback missing cached preview:\n%s", output)
	}
	if !strings.Contains(output, "Ctrl-]") || !strings.Contains(output, "q/Ctrl-C/Ctrl-Q quit") {
		t.Fatalf("active fallback missing recovery shortcuts:\n%s", output)
	}
}

func TestRenderActiveLeavesWaitingAfterAnyStreamOutput(t *testing.T) {
	app := NewApp(Client{HubURL: "http://hub", Token: "token"}, AppAuthResult{}, nil, &bytes.Buffer{})
	app.active = "local/demo"
	app.fullscreen = true
	app.streams["local/demo"] = &appSessionStream{
		sessionID: "local/demo",
		stream:    &Stream{},
		view:      terminalview.New(20, 5),
		size:      protocol.TerminalSize{Cols: 20, Rows: 5},
		writes:    make(chan appStreamWrite, 1),
	}
	app.queueSessionOutput("local/demo", []byte("\x1b[2J\x1b[H"))
	if !app.processPendingStreamOutput(1024) {
		t.Fatalf("expected pending output to process")
	}
	app.renderWithSize(100, 5)
	output := stripControl(app.Out.(*bytes.Buffer).String())
	if strings.Contains(output, "waiting for terminal output") {
		t.Fatalf("active fallback should be removed after stream output:\n%s", output)
	}
}

func TestActiveWaitingQQuitsUntilStreamOutput(t *testing.T) {
	app := NewApp(Client{HubURL: "http://hub", Token: "token"}, AppAuthResult{}, nil, &bytes.Buffer{})
	app.active = "local/demo"
	app.streams["local/demo"] = &appSessionStream{
		sessionID: "local/demo",
		view:      terminalview.New(20, 5),
		writes:    make(chan appStreamWrite, 1),
	}
	if !app.handleKey(t.Context(), "q") {
		t.Fatalf("expected q to quit while active terminal is still waiting for output")
	}

	writes := make(chan appStreamWrite, 1)
	app.active = "local/demo"
	app.streams["local/demo"] = &appSessionStream{
		sessionID:  "local/demo",
		view:       terminalview.New(20, 5),
		writes:     writes,
		seenOutput: true,
	}
	if app.handleKey(t.Context(), "q") {
		t.Fatalf("q should be forwarded after terminal stream output")
	}
	select {
	case write := <-writes:
		if write.data != "q" {
			t.Fatalf("unexpected forwarded input: %+v", write)
		}
	default:
		t.Fatalf("expected q to be forwarded to active stream")
	}
}

func TestActiveWaitingEscapeDetaches(t *testing.T) {
	app := NewApp(Client{HubURL: "http://hub", Token: "token"}, AppAuthResult{}, nil, &bytes.Buffer{})
	app.sessions = []protocol.SessionView{{ID: "local/demo"}}
	app.active = "local/demo"
	app.streams["local/demo"] = &appSessionStream{
		sessionID: "local/demo",
		view:      terminalview.New(20, 5),
		writes:    make(chan appStreamWrite, 1),
	}
	if app.handleKey(t.Context(), "unknown") {
		t.Fatalf("escape should detach instead of quitting")
	}
	if app.active != "" {
		t.Fatalf("expected escape to detach while waiting for terminal output")
	}
}

func TestActiveWaitingCtrlCQuits(t *testing.T) {
	app := NewApp(Client{HubURL: "http://hub", Token: "token"}, AppAuthResult{}, nil, &bytes.Buffer{})
	app.active = "local/demo"
	app.streams["local/demo"] = &appSessionStream{
		sessionID: "local/demo",
		view:      terminalview.New(20, 5),
		writes:    make(chan appStreamWrite, 1),
	}
	if !app.handleKey(t.Context(), "ctrl-c") {
		t.Fatalf("expected ctrl-c to quit while waiting for terminal output")
	}
}

func TestLoadSelectedPreviewUsesCache(t *testing.T) {
	app := &App{
		sessions: []protocol.SessionView{
			{ID: "local/a"},
			{ID: "local/b"},
		},
		selected: 1,
		previews: map[string]string{"local/b": "cached preview"},
	}
	app.loadSelectedPreview()
	if app.preview != "cached preview" {
		t.Fatalf("expected cached preview, got %q", app.preview)
	}
}

func TestLoadSelectedPreviewPrefersStreamBuffer(t *testing.T) {
	app := &App{
		sessions: []protocol.SessionView{{ID: "local/a"}},
		previews: map[string]string{"local/a": "capture"},
		buffers:  map[string]string{"local/a": "live"},
	}
	app.loadSelectedPreview()
	if app.preview != "live" {
		t.Fatalf("expected live buffer, got %q", app.preview)
	}
}

func TestLoadSelectedPreviewKeepsCaptureDuringWarmup(t *testing.T) {
	app := NewApp(Client{HubURL: "http://hub", Token: "token"}, AppAuthResult{}, nil, &bytes.Buffer{})
	app.sessions = []protocol.SessionView{{ID: "local/a"}}
	app.previews["local/a"] = "capture"
	app.buffers["local/a"] = "partial history"
	app.streams["local/a"] = &appSessionStream{
		sessionID:  "local/a",
		warming:    true,
		warmUntil:  time.Now().Add(time.Second),
		quietUntil: time.Now().Add(time.Second),
		maxWarm:    time.Now().Add(time.Second),
	}
	app.loadSelectedPreview()
	if app.preview != "capture" {
		t.Fatalf("expected capture during warmup, got %q", app.preview)
	}
	if !app.finishWarmStreams(time.Now().Add(2 * time.Second)) {
		t.Fatalf("expected warmup completion to change render state")
	}
	if app.preview != "partial history" {
		t.Fatalf("expected live buffer after warmup, got %q", app.preview)
	}
}

func TestAppendSessionBufferUsesTerminalView(t *testing.T) {
	app := NewApp(Client{HubURL: "http://hub", Token: "token"}, AppAuthResult{}, nil, &bytes.Buffer{})
	app.streams["local/a"] = &appSessionStream{
		sessionID: "local/a",
		view:      terminalview.New(40, 4),
	}
	app.queueSessionOutput("local/a", []byte("old line\n\x1b[2J\x1b[Hnew line"))
	if !app.processPendingStreamOutput(1024) {
		t.Fatalf("expected pending output to process")
	}
	if !app.refreshDirtyTerminalViews() {
		t.Fatalf("expected dirty terminal view")
	}
	got := stripControl(app.buffers["local/a"])
	if strings.Contains(got, "old line") {
		t.Fatalf("expected terminal clear to remove old content, got %q", got)
	}
	if !strings.Contains(got, "new line") {
		t.Fatalf("expected rendered terminal content, got %q", got)
	}
}

func TestProcessPendingStreamOutputIsBounded(t *testing.T) {
	app := NewApp(Client{HubURL: "http://hub", Token: "token"}, AppAuthResult{}, nil, &bytes.Buffer{})
	app.streams["local/a"] = &appSessionStream{
		sessionID: "local/a",
		view:      terminalview.New(40, 4),
		size:      protocol.TerminalSize{Cols: 40, Rows: 4},
	}
	app.queueSessionOutput("local/a", []byte(strings.Repeat("x", 128)))
	if !app.processPendingStreamOutput(32) {
		t.Fatalf("expected pending output to process")
	}
	if got := len(app.streams["local/a"].pending); got != 96 {
		t.Fatalf("expected bounded pending output, got %d bytes", got)
	}
}

func TestResizeSettleSuppressesIntermediateStreamRepaint(t *testing.T) {
	app := NewApp(Client{HubURL: "http://hub", Token: "token"}, AppAuthResult{}, nil, &bytes.Buffer{})
	app.sessions = []protocol.SessionView{{ID: "local/a"}}
	size := app.streamSizeFor("local/a")
	stream := &appSessionStream{
		sessionID: "local/a",
		view:      terminalview.New(size.Cols, size.Rows),
		size:      size,
	}
	app.streams["local/a"] = stream
	app.startResizeSettle(stream, time.Now())

	app.events <- appStreamEvent{sessionID: "local/a", data: []byte("resize repaint")}
	if app.drainStreamEvents(16) {
		t.Fatalf("resize repaint output should not request an immediate render")
	}
	if !stream.resizeSawOutput {
		t.Fatalf("expected resize settle to observe stream output")
	}
	if app.processPendingStreamOutput(1024) {
		t.Fatalf("processed resize repaint should not request an immediate render")
	}
	if !stream.viewDirty {
		t.Fatalf("expected terminal view to keep final repaint dirty")
	}
	if app.refreshDirtyTerminalViews() {
		t.Fatalf("dirty resize repaint should not refresh while settling")
	}
	if strings.Contains(app.buffers["local/a"], "resize repaint") {
		t.Fatalf("resize repaint leaked into visible buffer before settle")
	}
	if app.finishResizeSettles(stream.resizeQuiet.Add(-time.Millisecond)) {
		t.Fatalf("resize settle should wait for quiet window")
	}
	if !app.finishResizeSettles(stream.resizeQuiet.Add(time.Millisecond)) {
		t.Fatalf("resize settle should finish after quiet window")
	}
	if !app.refreshDirtyTerminalViews() {
		t.Fatalf("expected final repaint to refresh after settle")
	}
	if !strings.Contains(stripControl(app.buffers["local/a"]), "resize repaint") {
		t.Fatalf("final repaint missing after settle: %q", app.buffers["local/a"])
	}
}

func TestResizeSettleWaitsForResizeWriteBeforeQuietWindow(t *testing.T) {
	app := NewApp(Client{HubURL: "http://hub", Token: "token"}, AppAuthResult{}, nil, &bytes.Buffer{})
	stream := &appSessionStream{
		sessionID: "local/a",
		stream:    &Stream{},
		view:      terminalview.New(40, 4),
		size:      protocol.TerminalSize{Cols: 40, Rows: 4},
	}
	app.streams["local/a"] = stream
	now := time.Now()
	app.beginResizeSettle(stream, now)
	if !stream.resizeSettling || !stream.resizeAwaitingWrite {
		t.Fatalf("expected resize settle to wait for websocket write")
	}
	if app.finishResizeSettles(now.Add(appStreamResizeSettleMax - time.Millisecond)) {
		t.Fatalf("resize settle should not finish before queued write or max")
	}
	app.events <- appStreamEvent{sessionID: "local/a", stream: stream.stream, resizeSent: true}
	if app.drainStreamEvents(16) {
		t.Fatalf("resizeSent should not request an immediate render")
	}
	if stream.resizeAwaitingWrite {
		t.Fatalf("resizeSent should leave awaiting-write state")
	}
	if stream.resizeMax.Before(now.Add(appStreamResizeSettleMax)) || stream.resizeMax.Equal(now.Add(appStreamResizeSettleMax)) {
		t.Fatalf("resizeSent should restart settle max window")
	}
}

func TestResizeSettleFinishesWithoutOutputAfterMaxWait(t *testing.T) {
	app := NewApp(Client{HubURL: "http://hub", Token: "token"}, AppAuthResult{}, nil, &bytes.Buffer{})
	stream := &appSessionStream{
		sessionID: "local/a",
		view:      terminalview.New(40, 4),
		size:      protocol.TerminalSize{Cols: 40, Rows: 4},
	}
	app.streams["local/a"] = stream
	now := time.Now()
	app.startResizeSettle(stream, now)
	if app.finishResizeSettles(now.Add(appStreamResizeSettleMax - time.Millisecond)) {
		t.Fatalf("resize settle should wait for max window when no output arrives")
	}
	if !app.finishResizeSettles(now.Add(appStreamResizeSettleMax + time.Millisecond)) {
		t.Fatalf("resize settle should finish after max window without output")
	}
	if stream.resizeSettling {
		t.Fatalf("expected resize settle to clear")
	}
}

func TestForwardActiveKeyQueuesInputWithoutBlockingOnStreamWrite(t *testing.T) {
	app := NewApp(Client{HubURL: "http://hub", Token: "token"}, AppAuthResult{}, nil, &bytes.Buffer{})
	writes := make(chan appStreamWrite, 1)
	app.active = "local/a"
	app.streams["local/a"] = &appSessionStream{
		sessionID: "local/a",
		writes:    writes,
	}
	if err := app.forwardActiveKey("x"); err != nil {
		t.Fatal(err)
	}
	select {
	case write := <-writes:
		if write.data != "x" || write.resize {
			t.Fatalf("unexpected queued write: %+v", write)
		}
	default:
		t.Fatalf("expected input to be queued")
	}
}

func TestForwardActiveKeyPreservesUTF8InputBytes(t *testing.T) {
	app := NewApp(Client{HubURL: "http://hub", Token: "token"}, AppAuthResult{}, nil, &bytes.Buffer{})
	writes := make(chan appStreamWrite, 8)
	app.active = "local/a"
	app.streams["local/a"] = &appSessionStream{
		sessionID: "local/a",
		writes:    writes,
	}
	input := "中文"
	var wantWrites int
	for _, r := range input {
		wantWrites++
		if err := app.forwardActiveKey(string(r)); err != nil {
			t.Fatal(err)
		}
	}
	var got strings.Builder
	for i := 0; i < wantWrites; i++ {
		select {
		case write := <-writes:
			got.WriteString(write.data)
		default:
			t.Fatalf("expected forwarded utf-8 rune %d", i)
		}
	}
	if got.String() != input {
		t.Fatalf("forwarded utf-8 changed: got %q bytes=% x want %q bytes=% x", got.String(), []byte(got.String()), input, []byte(input))
	}
}

func TestHandleMouseSelectsSessionListRow(t *testing.T) {
	app := NewApp(Client{HubURL: "http://hub", Token: "token"}, AppAuthResult{}, nil, &bytes.Buffer{})
	app.sessions = []protocol.SessionView{{ID: "local/a"}, {ID: "local/b"}}
	app.selected = 0
	if !app.handleMouse(t.Context(), appMouseEvent{kind: appMouseClick, x: 2, y: 5, button: terminalview.MouseLeft}) {
		t.Fatalf("expected mouse click to change selection")
	}
	if app.selected != 1 {
		t.Fatalf("expected second session selected, got %d", app.selected)
	}
}

func TestHandleMouseDragUpdatesSplitWidth(t *testing.T) {
	app := NewApp(Client{HubURL: "http://hub", Token: "token"}, AppAuthResult{}, nil, &bytes.Buffer{})
	app.sessions = []protocol.SessionView{{ID: "local/a"}}
	if app.handleMouse(t.Context(), appMouseEvent{kind: appMouseClick, x: 31, y: 4, button: terminalview.MouseLeft}) {
		t.Fatalf("split drag start should not force render by itself")
	}
	if !app.dragSplit {
		t.Fatalf("expected split dragging to start")
	}
	if !app.handleMouse(t.Context(), appMouseEvent{kind: appMouseMotion, x: 44, y: 4, button: terminalview.MouseLeft}) {
		t.Fatalf("expected split drag motion to update layout")
	}
	if app.splitWidth != 43 {
		t.Fatalf("unexpected split width: %d", app.splitWidth)
	}
	if !app.handleMouse(t.Context(), appMouseEvent{kind: appMouseRelease, x: 44, y: 4, button: terminalview.MouseNone}) {
		t.Fatalf("expected split release to update UI")
	}
	if app.dragSplit {
		t.Fatalf("expected split dragging to stop")
	}
}

func TestHandleMouseDragDebouncesRemoteResizeUntilAfterRelease(t *testing.T) {
	app := NewApp(Client{HubURL: "http://hub", Token: "token"}, AppAuthResult{}, nil, &bytes.Buffer{})
	writes := make(chan appStreamWrite, 2)
	app.active = "local/a"
	app.sessions = []protocol.SessionView{{ID: "local/a"}}
	app.streams["local/a"] = &appSessionStream{
		sessionID:  "local/a",
		stream:     &Stream{},
		view:       terminalview.New(80, 17),
		size:       protocol.TerminalSize{Cols: 80, Rows: 17},
		writes:     writes,
		seenOutput: true,
	}
	if app.handleMouse(t.Context(), appMouseEvent{kind: appMouseClick, x: 31, y: 4, button: terminalview.MouseLeft}) {
		t.Fatalf("split drag start should not force render by itself")
	}
	if !app.handleMouse(t.Context(), appMouseEvent{kind: appMouseMotion, x: 44, y: 4, button: terminalview.MouseLeft}) {
		t.Fatalf("expected split drag motion to update layout")
	}
	stream := app.streams["local/a"]
	if !stream.resizePending {
		t.Fatalf("expected resize to be pending during drag")
	}
	select {
	case write := <-writes:
		t.Fatalf("resize should not be sent during drag: %+v", write)
	default:
	}
	app.flushDueStreamResizes(time.Now().Add(appStreamResizeDebounce + time.Second))
	select {
	case write := <-writes:
		t.Fatalf("resize should not flush while mouse is dragging: %+v", write)
	default:
	}
	if !app.handleMouse(t.Context(), appMouseEvent{kind: appMouseRelease, x: 44, y: 4, button: terminalview.MouseNone}) {
		t.Fatalf("expected split release to update UI")
	}
	app.flushDueStreamResizes(time.Now())
	select {
	case write := <-writes:
		t.Fatalf("resize should wait for debounce after release: %+v", write)
	default:
	}
	app.flushDueStreamResizes(time.Now().Add(appStreamResizeDebounce + time.Millisecond))
	select {
	case write := <-writes:
		if !write.resize {
			t.Fatalf("expected resize write, got %+v", write)
		}
		if write.size != (protocol.TerminalSize{Cols: 74, Rows: 29}) {
			t.Fatalf("unexpected resize size: %+v", write.size)
		}
	default:
		t.Fatalf("expected debounced resize write")
	}
}

func TestHandleMouseForwardsTerminalMouseInput(t *testing.T) {
	app := NewApp(Client{HubURL: "http://hub", Token: "token"}, AppAuthResult{}, nil, &bytes.Buffer{})
	writes := make(chan appStreamWrite, 1)
	view := terminalview.New(80, 17)
	view.Write([]byte("\x1b[?1000h\x1b[?1006h"))
	app.active = "local/a"
	app.sessions = []protocol.SessionView{{ID: "local/a"}}
	app.streams["local/a"] = &appSessionStream{
		sessionID:  "local/a",
		view:       view,
		size:       protocol.TerminalSize{Cols: 80, Rows: 17},
		writes:     writes,
		seenOutput: true,
	}
	if app.handleMouse(t.Context(), appMouseEvent{kind: appMouseClick, x: 40, y: 6, button: terminalview.MouseLeft}) {
		t.Fatalf("terminal mouse forwarding should not require local repaint")
	}
	select {
	case write := <-writes:
		if write.data != "\x1b[<0;7;3M" {
			t.Fatalf("unexpected mouse input sequence: %q", write.data)
		}
	default:
		t.Fatalf("expected mouse input to be forwarded")
	}
}

func TestCtrlQQuitsEvenWhenAttached(t *testing.T) {
	app := NewApp(Client{HubURL: "http://hub", Token: "token"}, AppAuthResult{}, nil, &bytes.Buffer{})
	app.active = "local/a"
	if !app.handleKey(t.Context(), "ctrl-q") {
		t.Fatalf("expected ctrl-q to quit from active mode")
	}
}

func TestCtrlFTogglesFullscreenWhenAttached(t *testing.T) {
	app := NewApp(Client{HubURL: "http://hub", Token: "token"}, AppAuthResult{}, nil, &bytes.Buffer{})
	app.active = "local/a"
	app.streams["local/a"] = &appSessionStream{sessionID: "local/a", view: terminalview.New(80, 24)}
	if app.handleKey(t.Context(), "ctrl-f") {
		t.Fatalf("ctrl-f should not quit")
	}
	if !app.fullscreen {
		t.Fatalf("expected ctrl-f to enter fullscreen")
	}
	if app.handleKey(t.Context(), "ctrl-f") {
		t.Fatalf("second ctrl-f should not quit")
	}
	if app.fullscreen {
		t.Fatalf("expected second ctrl-f to return to split pane")
	}
}

func TestDrainStreamEventsIsBounded(t *testing.T) {
	app := NewApp(Client{HubURL: "http://hub", Token: "token"}, AppAuthResult{}, nil, &bytes.Buffer{})
	app.sessions = []protocol.SessionView{{ID: "local/a"}}
	app.streams["local/a"] = &appSessionStream{
		sessionID: "local/a",
		view:      terminalview.New(40, 4),
		size:      protocol.TerminalSize{Cols: 40, Rows: 4},
	}
	for i := 0; i < 40; i++ {
		app.events <- appStreamEvent{sessionID: "local/a", data: []byte("x")}
	}
	if !app.drainStreamEvents(16) {
		t.Fatalf("expected events to drain")
	}
	if len(app.events) != 24 {
		t.Fatalf("expected bounded drain to leave events queued, got %d", len(app.events))
	}
}

func TestRenderDoesNotClearWholeScreenEachFrame(t *testing.T) {
	app := &App{
		Out: &bytes.Buffer{},
		sessions: []protocol.SessionView{{
			ID: "local/demo", WorkerID: "local", Name: "demo", Status: "active", Command: "bash",
		}},
		preview: "ready",
		status:  "ready",
	}
	app.renderWithSize(120, 24)
	output := app.Out.(*bytes.Buffer).String()
	if strings.Contains(output, "\x1b[2J") {
		t.Fatalf("render should not clear whole screen every frame")
	}
	if !strings.Contains(output, "\x1b[2K") {
		t.Fatalf("render should clear individual lines")
	}
}

func TestDetachMarksStreamForTTL(t *testing.T) {
	app := NewApp(Client{HubURL: "http://hub", Token: "token"}, AppAuthResult{}, nil, &bytes.Buffer{})
	app.streams["local/a"] = &appSessionStream{sessionID: "local/a"}
	app.sessions = []protocol.SessionView{{ID: "local/a"}}
	app.active = "local/a"
	app.fullscreen = true
	app.detachActive()
	if app.active != "" {
		t.Fatalf("expected active session to clear")
	}
	if app.fullscreen {
		t.Fatalf("expected detach to leave fullscreen")
	}
	if app.streams["local/a"].keepUntil.IsZero() {
		t.Fatalf("expected detach TTL")
	}
	if app.cleanupStreams(time.Now().Add(appStreamDetachTTL+time.Second)) != true {
		t.Fatalf("expected expired stream cleanup")
	}
	if _, ok := app.streams["local/a"]; ok {
		t.Fatalf("expected stream to close after TTL")
	}
}

type chunkReader struct {
	chunks [][]byte
	index  int
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if r.index >= len(r.chunks) {
		return 0, io.EOF
	}
	chunk := r.chunks[r.index]
	r.index++
	copy(p, chunk)
	return min(len(p), len(chunk)), nil
}
