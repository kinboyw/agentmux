package control

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mattn/go-runewidth"

	"private/agentmux/internal/protocol"
	"private/agentmux/internal/term"
	"private/agentmux/internal/terminalview"
)

type App struct {
	Client Client
	Auth   AppAuthResult
	In     *os.File
	Out    io.Writer

	workers    []protocol.WorkerView
	sessions   []protocol.SessionView
	selected   int
	status     string
	err        error
	preview    string
	previews   map[string]string
	loggedIn   bool
	streams    map[string]*appSessionStream
	buffers    map[string]string
	active     string
	fullscreen bool
	events     chan appStreamEvent
	splitWidth int
	dragSplit  bool

	rawRestore func()
	keys       appKeyReader
	debug      appDebugState
}

const (
	appStreamBufferLimit         = 256 * 1024
	appStreamPendingLimit        = 4 * 1024 * 1024
	appStreamProcessBytesPerLoop = 32 * 1024
	appStreamWriteQueueLimit     = 256
	appStreamDetachTTL           = 90 * time.Second
	appStreamWarmupMin           = 450 * time.Millisecond
	appStreamWarmupQuiet         = 180 * time.Millisecond
	appStreamWarmupMax           = 1500 * time.Millisecond
	appStreamResizeDebounce      = 500 * time.Millisecond
	appStreamResizeSettleQuiet   = 280 * time.Millisecond
	appStreamResizeSettleMax     = 1600 * time.Millisecond
	appRenderMinInterval         = 120 * time.Millisecond
)

type appSessionStream struct {
	sessionID           string
	stream              *Stream
	view                *terminalview.View
	viewDirty           bool
	size                protocol.TerminalSize
	pending             []byte
	writes              chan appStreamWrite
	cancel              context.CancelFunc
	connecting          bool
	closing             bool
	keepUntil           time.Time
	warming             bool
	warmUntil           time.Time
	quietUntil          time.Time
	maxWarm             time.Time
	resizeDue           time.Time
	resizeQuiet         time.Time
	resizeMax           time.Time
	seenOutput          bool
	visibleOutput       bool
	prefilled           bool
	resizePending       bool
	resizeSettling      bool
	resizeAwaitingWrite bool
	resizeSawOutput     bool
	pendingResize       protocol.TerminalSize
}

type appStreamEvent struct {
	sessionID  string
	stream     *Stream
	connected  bool
	data       []byte
	err        error
	closed     bool
	resizeSent bool
}

type appStreamWrite struct {
	data   string
	size   protocol.TerminalSize
	resize bool
}

type appInputEvent struct {
	key   string
	mouse *appMouseEvent
}

type appMouseKind int

const (
	appMouseClick appMouseKind = iota + 1
	appMouseRelease
	appMouseMotion
	appMouseWheel
)

type appMouseEvent struct {
	kind   appMouseKind
	x      int
	y      int
	button terminalview.MouseButton
	shift  bool
	alt    bool
	ctrl   bool
}

func NewApp(client Client, auth AppAuthResult, in *os.File, out io.Writer) *App {
	return &App{
		Client:   client,
		Auth:     auth,
		In:       in,
		Out:      out,
		loggedIn: client.HubURL != "" && client.Token != "",
		previews: map[string]string{},
		buffers:  map[string]string{},
		streams:  map[string]*appSessionStream{},
		events:   make(chan appStreamEvent, 128),
	}
}

func NewUnauthApp(in *os.File, out io.Writer, err error) *App {
	app := NewApp(Client{}, AppAuthResult{}, in, out)
	if err != nil {
		app.err = err
	}
	return app
}

func (a *App) Run(ctx context.Context) error {
	defer a.closeDebug()
	a.debugf("run start logged_in=%t hub=%q source=%q", a.loggedIn, debugSafeURL(a.Client.HubURL), a.Auth.Source)
	defer a.debugf("run stop")
	if a.In == nil {
		a.In = os.Stdin
	}
	if a.Out == nil {
		a.Out = os.Stdout
	}
	if err := a.enableRaw(); err != nil {
		return err
	}
	defer a.disableRaw()
	term.EnterAlternateScreen(a.Out)
	term.EnableMouseCellMotion(a.Out)
	clear(a.Out)
	defer func() {
		term.ResetModes(a.Out)
		term.ExitAlternateScreen(a.Out)
	}()
	if a.loggedIn {
		a.status = "ready"
		a.refresh(ctx)
	} else {
		a.status = "not logged in; type /login"
	}
	a.render()
	lastCleanup := time.Now()
	lastRender := time.Now()
	pendingRender := false
	for {
		input, ok, err := a.keys.ReadEventAvailable(a.In)
		if err != nil {
			return err
		}
		changed := false
		if ok {
			if input.mouse != nil {
				changed = a.handleMouse(ctx, *input.mouse)
			} else {
				a.recordDebugKey(input.key, a.active != "")
				quit := a.handleKey(ctx, input.key)
				a.clampSelection()
				changed = true
				if quit {
					return nil
				}
			}
		}
		changed = a.drainStreamEvents(16) || changed
		changed = a.processPendingStreamOutput(appStreamProcessBytesPerLoop) || changed
		changed = a.flushDueStreamResizes(time.Now()) || changed
		changed = a.finishResizeSettles(time.Now()) || changed
		if time.Since(lastCleanup) >= time.Second {
			lastCleanup = time.Now()
			changed = a.cleanupStreams(time.Now()) || changed
		}
		changed = a.finishWarmStreams(time.Now()) || changed
		if changed {
			pendingRender = true
		}
		if pendingRender && time.Since(lastRender) >= appRenderMinInterval {
			a.refreshDirtyTerminalViews()
			a.render()
			lastRender = time.Now()
			pendingRender = false
		}
	}
}

func (a *App) handleKey(ctx context.Context, key string) bool {
	if key == "ctrl-q" {
		a.closeAllStreams()
		return true
	}
	if a.debugEnabled() && (key == "ctrl-g" || (a.active == "" && key == "D")) {
		a.writeDebugSnapshotStatus("manual")
		return false
	}
	if a.active != "" {
		if key == "ctrl-f" {
			a.toggleFullscreen()
			return false
		}
		if key == "detach" {
			a.detachActive()
			return false
		}
		if a.activeWaitingForStreamOutput() {
			switch key {
			case "unknown":
				a.detachActive()
				return false
			case "q", "Q", "ctrl-c":
				a.closeAllStreams()
				return true
			}
		}
		if err := a.forwardActiveKey(key); err != nil {
			a.err = err
			a.status = "input failed"
		}
		return false
	}
	switch key {
	case "q":
		a.closeAllStreams()
		return true
	case "up", "k":
		if a.selected > 0 {
			a.selected--
			a.loadSelectedPreview()
		}
	case "down", "j":
		if a.selected < len(a.sessions)-1 {
			a.selected++
			a.loadSelectedPreview()
		}
	case "/":
		a.promptSlash(ctx)
	case "r":
		a.refresh(ctx)
	case "c":
		a.promptCreate(ctx)
	case "s":
		a.promptSend(ctx)
	case "x":
		a.promptStop(ctx)
	case "enter", "a":
		if err := a.attach(ctx); err != nil {
			a.err = err
			a.status = "attach failed"
		}
	case "ctrl-f":
		if err := a.attach(ctx); err != nil {
			a.err = err
			a.status = "attach failed"
			return false
		}
		a.enterFullscreen()
	case "?":
		a.status = "keys: up/down select, enter attach, Ctrl-F fullscreen, Ctrl-] detach, / commands, c create, s send, x stop, r refresh, q quit"
		if a.debugEnabled() {
			a.status = "keys: up/down select, enter attach, Ctrl-F fullscreen, Ctrl-] detach, / commands, c create, s send, x stop, r refresh, D debug, Ctrl-G debug attached, q quit"
		}
	}
	return false
}

func (a *App) handleMouse(ctx context.Context, event appMouseEvent) bool {
	if event.x < 1 || event.y < 1 {
		return false
	}
	if a.fullscreen && a.active != "" {
		return a.forwardActiveMouse(event, 1, 1)
	}
	cols, rows := 120, 36
	if a.In != nil {
		if c, r, err := term.Size(a.In); err == nil {
			cols, rows = c, r
		}
	}
	listWidth, previewWidth, limit := a.layoutForSize(cols, rows)
	bodyStart := 4
	bodyEnd := bodyStart + limit - 1
	if event.kind == appMouseClick && event.button == terminalview.MouseLeft && previewWidth > 0 && event.x >= listWidth+1 && event.x <= listWidth+3 && event.y >= bodyStart && event.y <= bodyEnd {
		a.dragSplit = true
		return false
	}
	if event.kind == appMouseMotion && a.dragSplit {
		a.setSplitWidth(event.x-1, cols)
		if stream := a.streams[a.active]; stream != nil {
			a.resizeStreamView(stream)
		}
		a.loadSelectedPreview()
		return true
	}
	if event.kind == appMouseRelease && a.dragSplit {
		a.dragSplit = false
		if stream := a.streams[a.active]; stream != nil {
			a.delayPendingStreamResize(stream, time.Now())
		}
		return true
	}
	if event.kind == appMouseClick && event.button == terminalview.MouseLeft && event.y >= bodyStart && event.y <= bodyEnd && event.x <= listWidth {
		start := scrollStart(a.selected, limit, len(a.sessions))
		index := start + event.y - bodyStart
		if index >= 0 && index < len(a.sessions) {
			a.selected = index
			a.loadSelectedPreview()
			return true
		}
	}
	if event.x >= listWidth+4 && event.y >= bodyStart && event.y <= bodyEnd {
		if a.active != "" {
			return a.forwardActiveMouse(event, listWidth+4, bodyStart)
		}
		if event.kind == appMouseClick && event.button == terminalview.MouseLeft {
			if err := a.attach(ctx); err != nil {
				a.err = err
				a.status = "attach failed"
			}
			return true
		}
	}
	return false
}

func (a *App) forwardActiveMouse(event appMouseEvent, originX, originY int) bool {
	if a.active == "" {
		return false
	}
	stream := a.streams[a.active]
	if stream == nil || stream.view == nil {
		return false
	}
	data := stream.view.MouseInput(terminalview.MouseEvent{
		X:       event.x - originX,
		Y:       event.y - originY,
		Button:  event.button,
		Motion:  event.kind == appMouseMotion,
		Release: event.kind == appMouseRelease,
		Shift:   event.shift,
		Alt:     event.alt,
		Ctrl:    event.ctrl,
	})
	if data == "" {
		return false
	}
	if stream.writes != nil {
		select {
		case stream.writes <- appStreamWrite{data: data}:
			return false
		default:
			a.status = "input queue full " + a.active
			return true
		}
	}
	if stream.stream != nil {
		if err := stream.stream.Input(data); err != nil {
			a.err = err
			a.status = "mouse input failed"
			return true
		}
	}
	return false
}

func (a *App) writeDebugSnapshotStatus(reason string) {
	path, err := a.writeDebugSnapshot(reason)
	if err != nil {
		a.err = err
		a.status = "debug snapshot failed"
		return
	}
	a.err = nil
	a.status = "debug snapshot " + path
}

func (a *App) refresh(ctx context.Context) {
	if !a.ensureLoggedIn() {
		return
	}
	a.debugf("refresh start")
	a.ensureAppState()
	workers, err := a.Client.Workers(ctx)
	if err != nil {
		a.err = err
		a.status = "refresh workers failed"
		a.debugf("refresh workers failed error=%q", err.Error())
		return
	}
	sessions, err := a.Client.Sessions(ctx)
	if err != nil {
		a.err = err
		a.status = "refresh sessions failed"
		a.debugf("refresh sessions failed error=%q", err.Error())
		return
	}
	a.workers = dedupeWorkers(workers)
	a.sessions = dedupeSessions(sessions)
	a.err = nil
	a.status = fmt.Sprintf("refreshed %s", time.Now().Format("15:04:05"))
	a.clampSelection()
	a.closeMissingSessionStreams()
	a.refreshPreviewCache(ctx)
	a.loadSelectedPreview()
	a.debugf("refresh complete workers=%d sessions=%d selected=%d", len(a.workers), len(a.sessions), a.selected)
}

func (a *App) refreshPreviewCache(ctx context.Context) {
	a.previews = map[string]string{}
	previewLines := a.previewCaptureLines()
	type result struct {
		id   string
		data string
	}
	jobs := make(chan protocol.SessionView)
	results := make(chan result, len(a.sessions))
	workers := min(4, max(1, len(a.sessions)))
	for i := 0; i < workers; i++ {
		go func() {
			for session := range jobs {
				if session.ID == "" {
					continue
				}
				data, err := a.Client.SessionPreview(ctx, session.ID, previewLines)
				if err != nil {
					results <- result{id: session.ID}
					continue
				}
				results <- result{id: session.ID, data: data}
			}
		}()
	}
	sent := 0
	for _, session := range a.sessions {
		if session.ID == "" {
			continue
		}
		jobs <- session
		sent++
	}
	close(jobs)
	for i := 0; i < sent; i++ {
		item := <-results
		a.previews[item.id] = item.data
	}
}

func (a *App) previewCaptureLines() int {
	cols, rows := 120, 36
	if a != nil && a.In != nil {
		if c, r, err := term.Size(a.In); err == nil {
			cols, rows = c, r
		}
	}
	_, _, limit := a.layoutForSize(cols, rows)
	return max(4, limit)
}

func (a *App) loadSelectedPreview() {
	session := a.selectedSession()
	if session.ID == "" {
		a.preview = ""
		return
	}
	if stream := a.streams[session.ID]; stream != nil && stream.warming {
		a.preview = a.previews[session.ID]
		return
	}
	if a.buffers != nil && a.buffers[session.ID] != "" {
		a.preview = a.buffers[session.ID]
		return
	}
	a.preview = a.previews[session.ID]
}

func (a *App) ensureAppState() {
	if a.previews == nil {
		a.previews = map[string]string{}
	}
	if a.buffers == nil {
		a.buffers = map[string]string{}
	}
	if a.streams == nil {
		a.streams = map[string]*appSessionStream{}
	}
	if a.events == nil {
		a.events = make(chan appStreamEvent, 128)
	}
}

func (a *App) closeMissingSessionStreams() {
	a.ensureAppState()
	current := map[string]bool{}
	for _, session := range a.sessions {
		current[session.ID] = true
	}
	for sessionID := range a.streams {
		if current[sessionID] {
			continue
		}
		a.closeSessionStream(sessionID)
		if a.active == sessionID {
			a.active = ""
			a.fullscreen = false
		}
	}
}

func (a *App) promptCreate(ctx context.Context) {
	if !a.ensureLoggedIn() {
		return
	}
	a.leaveRawForPrompt()
	defer a.enterRawAfterPrompt()
	fmt.Fprintln(a.Out)
	workerID := a.promptLine("worker", defaultWorkerID(a.workers))
	name := a.promptLine("name", "")
	cwd := a.promptLine("cwd", ".")
	command := a.promptLine("command", "bash")
	if strings.TrimSpace(workerID) == "" || strings.TrimSpace(name) == "" || strings.TrimSpace(command) == "" {
		a.status = "create canceled"
		return
	}
	req := protocol.CreateSession{WorkerID: workerID, Name: name, CWD: cwd, Command: command}
	if err := a.Client.CreateSession(ctx, req); err != nil {
		a.err = err
		a.status = "create failed"
		return
	}
	a.status = "session created"
	time.Sleep(300 * time.Millisecond)
	a.refresh(ctx)
}

func (a *App) promptSend(ctx context.Context) {
	if !a.ensureLoggedIn() {
		return
	}
	session := a.selectedSession()
	if session.ID == "" {
		a.status = "no session selected"
		return
	}
	a.leaveRawForPrompt()
	defer a.enterRawAfterPrompt()
	fmt.Fprintln(a.Out)
	text := a.promptLine("text", "")
	if text == "" {
		a.status = "send canceled"
		return
	}
	if err := a.Client.SendInput(ctx, session.ID, text+"\n"); err != nil {
		a.err = err
		a.status = "send failed"
		return
	}
	a.status = "input queued"
}

func (a *App) promptStop(ctx context.Context) {
	if !a.ensureLoggedIn() {
		return
	}
	session := a.selectedSession()
	if session.ID == "" {
		a.status = "no session selected"
		return
	}
	a.leaveRawForPrompt()
	defer a.enterRawAfterPrompt()
	fmt.Fprintln(a.Out)
	confirm := a.promptLine("stop "+session.ID+"? type yes", "")
	if confirm != "yes" {
		a.status = "stop canceled"
		return
	}
	if err := a.Client.StopSession(ctx, session.ID); err != nil {
		a.err = err
		a.status = "stop failed"
		return
	}
	a.status = "stop queued"
	time.Sleep(300 * time.Millisecond)
	a.refresh(ctx)
}

func (a *App) attach(ctx context.Context) error {
	if !a.ensureLoggedIn() {
		return nil
	}
	session := a.selectedSession()
	if session.ID == "" {
		a.status = "no session selected"
		return nil
	}
	a.ensureAppState()
	previousActive := a.active
	previousFullscreen := a.fullscreen
	a.active = session.ID
	a.fullscreen = false
	if err := a.ensureStream(ctx, session.ID); err != nil {
		a.active = previousActive
		a.fullscreen = previousFullscreen
		return err
	}
	if stream := a.streams[session.ID]; stream != nil {
		stream.keepUntil = time.Time{}
		a.resizeStreamView(stream)
	}
	a.status = attachedPreviewStatus(session.ID)
	a.loadSelectedPreview()
	a.debugf("attach session=%q", session.ID)
	return nil
}

func (a *App) enterFullscreen() {
	if a.active == "" {
		return
	}
	a.fullscreen = true
	if stream := a.streams[a.active]; stream != nil {
		a.resizeStreamView(stream)
	}
	a.status = attachedFullscreenStatus(a.active)
	a.debugf("fullscreen enter session=%q", a.active)
}

func (a *App) exitFullscreen() {
	if a.active == "" {
		a.fullscreen = false
		return
	}
	a.fullscreen = false
	if stream := a.streams[a.active]; stream != nil {
		a.resizeStreamView(stream)
	}
	a.status = attachedPreviewStatus(a.active)
	a.loadSelectedPreview()
	a.debugf("fullscreen exit session=%q", a.active)
}

func (a *App) toggleFullscreen() {
	if a.fullscreen {
		a.exitFullscreen()
		return
	}
	a.enterFullscreen()
}

func (a *App) detachActive() {
	if a.active == "" {
		return
	}
	sessionID := a.active
	a.active = ""
	a.fullscreen = false
	if stream := a.streams[sessionID]; stream != nil {
		stream.keepUntil = time.Now().Add(appStreamDetachTTL)
		a.resizeStreamView(stream)
	}
	a.status = "detached " + sessionID + "  stream kept " + appStreamDetachTTL.String()
	a.loadSelectedPreview()
	a.debugf("detach session=%q keep_until=%s", sessionID, streamKeepUntil(a.streams[sessionID]))
}

func (a *App) forwardActiveKey(key string) error {
	sessionID := a.active
	if sessionID == "" {
		return nil
	}
	stream := a.streams[sessionID]
	if stream == nil {
		a.status = "stream connecting " + sessionID
		return nil
	}
	data := appKeyInputData(key)
	if data == "" {
		return nil
	}
	if stream.writes != nil {
		select {
		case stream.writes <- appStreamWrite{data: data}:
			return nil
		default:
			a.status = "input queue full " + sessionID
			return nil
		}
	}
	if stream.stream == nil {
		a.status = "stream connecting " + sessionID
		return nil
	}
	return stream.stream.Input(data)
}

func appKeyInputData(key string) string {
	switch key {
	case "enter":
		return "\r"
	case "up":
		return "\x1b[A"
	case "down":
		return "\x1b[B"
	case "right":
		return "\x1b[C"
	case "left":
		return "\x1b[D"
	case "delete":
		return "\x1b[3~"
	case "tab":
		return "\t"
	case "ctrl-c":
		return "\x03"
	case "unknown":
		return "\x1b"
	case "detach":
		return ""
	case "ctrl-g":
		return "\x07"
	default:
		if appKeyIsSingleRune(key) {
			return key
		}
		return ""
	}
}

func appKeyIsSingleRune(key string) bool {
	if key == "" || !utf8.ValidString(key) {
		return false
	}
	_, size := utf8.DecodeRuneInString(key)
	return size == len(key)
}

func (a *App) ensureStream(ctx context.Context, sessionID string) error {
	if !a.ensureLoggedIn() {
		return nil
	}
	a.ensureAppState()
	if existing := a.streams[sessionID]; existing != nil {
		return nil
	}
	a.debugf("stream ensure session=%q", sessionID)
	streamCtx, cancel := context.WithCancel(ctx)
	now := time.Now()
	state := &appSessionStream{
		sessionID: sessionID, cancel: cancel, connecting: true, warming: true,
		writes:    make(chan appStreamWrite, appStreamWriteQueueLimit),
		warmUntil: now.Add(appStreamWarmupMin), quietUntil: now.Add(appStreamWarmupQuiet), maxWarm: now.Add(appStreamWarmupMax),
	}
	a.streams[sessionID] = state
	size := a.streamSizeFor(sessionID)
	state.view = terminalview.New(size.Cols, size.Rows)
	state.size = size
	if capture := a.previews[sessionID]; capture != "" && a.active != sessionID {
		state.view.Write([]byte(capture))
		state.viewDirty = true
		state.prefilled = true
		a.updateStreamBuffer(state)
	}
	go a.openStream(streamCtx, sessionID, size, state.writes)
	return nil
}

func (a *App) openStream(ctx context.Context, sessionID string, size protocol.TerminalSize, writes <-chan appStreamWrite) {
	a.debugf("stream open start session=%q cols=%d rows=%d", sessionID, size.Cols, size.Rows)
	stream, err := a.Client.OpenStream(ctx, sessionID, size)
	if err != nil {
		a.debugf("stream open failed session=%q error=%q", sessionID, err.Error())
		a.sendStreamEvent(appStreamEvent{sessionID: sessionID, err: err, closed: true})
		return
	}
	a.debugf("stream open complete session=%q stream_id=%q", sessionID, stream.StreamID)
	done := make(chan struct{})
	defer close(done)
	go a.writeStreamLoop(ctx, sessionID, stream, writes, done)
	a.sendStreamEvent(appStreamEvent{sessionID: sessionID, stream: stream, connected: true})
	for {
		event, err := stream.ReadEvent()
		if len(event.Data) > 0 {
			a.debugf("stream read output session=%q stream_id=%q bytes=%d", sessionID, stream.StreamID, len(event.Data))
			a.sendStreamEvent(appStreamEvent{sessionID: sessionID, stream: stream, data: event.Data})
		}
		if event.Err != nil {
			a.debugf("stream read event error session=%q stream_id=%q error=%q", sessionID, stream.StreamID, event.Err.Error())
			a.sendStreamEvent(appStreamEvent{sessionID: sessionID, stream: stream, err: event.Err})
		}
		if err != nil {
			a.debugf("stream read ended session=%q stream_id=%q error=%q", sessionID, stream.StreamID, err.Error())
			if !errors.Is(err, context.Canceled) {
				a.sendStreamEvent(appStreamEvent{sessionID: sessionID, stream: stream, err: err, closed: true})
			} else {
				a.sendStreamEvent(appStreamEvent{sessionID: sessionID, stream: stream, closed: true})
			}
			return
		}
	}
}

func (a *App) writeStreamLoop(ctx context.Context, sessionID string, stream *Stream, writes <-chan appStreamWrite, done <-chan struct{}) {
	for {
		select {
		case <-ctx.Done():
			a.debugf("stream write loop context done session=%q stream_id=%q", sessionID, stream.StreamID)
			return
		case <-done:
			a.debugf("stream write loop done session=%q stream_id=%q", sessionID, stream.StreamID)
			return
		case write := <-writes:
			var err error
			if write.resize {
				a.debugf("stream write resize session=%q stream_id=%q cols=%d rows=%d", sessionID, stream.StreamID, write.size.Cols, write.size.Rows)
				err = stream.Resize(write.size)
				if err == nil {
					a.sendStreamEvent(appStreamEvent{sessionID: sessionID, stream: stream, resizeSent: true})
				}
			} else if write.data != "" {
				a.debugf("stream write input session=%q stream_id=%q bytes=%d", sessionID, stream.StreamID, len(write.data))
				err = stream.Input(write.data)
			}
			if err != nil {
				a.debugf("stream write failed session=%q stream_id=%q error=%q", sessionID, stream.StreamID, err.Error())
				a.sendStreamEvent(appStreamEvent{sessionID: sessionID, stream: stream, err: err, closed: true})
				return
			}
		}
	}
}

func (a *App) sendStreamEvent(event appStreamEvent) {
	events := a.events
	if events == nil {
		return
	}
	select {
	case events <- event:
	default:
		if len(event.data) > 0 && !event.connected && !event.closed && event.err == nil {
			a.debugf("stream event dropped data session=%q bytes=%d", event.sessionID, len(event.data))
			return
		}
		select {
		case <-events:
			a.debugf("stream event queue dropped oldest")
		default:
		}
		select {
		case events <- event:
		default:
			a.debugf("stream event dropped session=%q connected=%t closed=%t err=%t", event.sessionID, event.connected, event.closed, event.err != nil)
		}
	}
}

func (a *App) drainStreamEvents(limit int) bool {
	a.ensureAppState()
	changed := false
	for i := 0; i < limit; i++ {
		select {
		case event := <-a.events:
			a.applyStreamEvent(event)
			if a.streamEventNeedsRender(event) {
				changed = true
			}
		default:
			return changed
		}
	}
	return changed
}

func (a *App) streamEventNeedsRender(event appStreamEvent) bool {
	if event.resizeSent {
		return false
	}
	if len(event.data) == 0 || event.connected || event.err != nil || event.closed {
		return true
	}
	stream := a.streams[event.sessionID]
	return stream == nil || !stream.resizeSettling
}

func (a *App) applyStreamEvent(event appStreamEvent) {
	a.ensureAppState()
	a.recordDebugStreamEvent(event)
	stream := a.streams[event.sessionID]
	if stream == nil {
		if event.stream != nil {
			go func(stream *Stream) {
				_ = stream.Close()
			}(event.stream)
		}
		return
	}
	if event.connected {
		stream.stream = event.stream
		stream.connecting = false
		a.resizeStreamView(stream)
		now := time.Now()
		stream.warming = true
		stream.warmUntil = now.Add(appStreamWarmupMin)
		stream.quietUntil = now.Add(appStreamWarmupQuiet)
		stream.maxWarm = now.Add(appStreamWarmupMax)
		if a.active == event.sessionID {
			a.status = "attached " + event.sessionID + "  warming stream"
		}
		if event.stream != nil {
			a.debugf("stream connected applied session=%q stream_id=%q size=%dx%d", event.sessionID, event.stream.StreamID, stream.size.Cols, stream.size.Rows)
		}
	}
	if event.resizeSent {
		if event.stream == nil || stream.stream == nil || stream.stream == event.stream {
			a.startResizeSettle(stream, time.Now())
		}
	}
	if len(event.data) > 0 {
		if stream.prefilled {
			oldView := stream.view
			stream.view = terminalview.New(stream.size.Cols, stream.size.Rows)
			stream.viewDirty = true
			stream.prefilled = false
			if oldView != nil {
				oldView.Close()
			}
		}
		stream.seenOutput = true
		if stream.resizeSettling {
			stream.resizeSawOutput = true
			stream.resizeQuiet = time.Now().Add(appStreamResizeSettleQuiet)
		}
		if event.stream != nil {
			a.debugf("stream output applied session=%q stream_id=%q bytes=%d pending_before=%d", event.sessionID, event.stream.StreamID, len(event.data), len(stream.pending))
		}
		a.queueSessionOutput(event.sessionID, event.data)
		if stream.warming {
			stream.quietUntil = time.Now().Add(appStreamWarmupQuiet)
		} else if a.selectedSession().ID == event.sessionID {
			a.loadSelectedPreview()
		}
	}
	if event.err != nil {
		if stream == nil || !stream.closing {
			a.err = event.err
			if a.active == event.sessionID {
				a.status = "stream error " + event.sessionID
			}
		}
	}
	if event.closed {
		if stream == nil {
			return
		}
		if event.stream == nil || stream.stream == nil || stream.stream == event.stream {
			delete(a.streams, event.sessionID)
			if stream.view != nil {
				stream.view.Close()
			}
			if a.active == event.sessionID {
				a.active = ""
				a.fullscreen = false
				a.status = "stream closed " + event.sessionID
			}
			a.debugf("stream closed session=%q", event.sessionID)
		}
	}
}

func (a *App) queueSessionOutput(sessionID string, data []byte) {
	if len(data) == 0 {
		return
	}
	a.ensureAppState()
	stream := a.streams[sessionID]
	if stream != nil {
		stream.seenOutput = true
		stream.pending = append(stream.pending, data...)
		if len(stream.pending) > appStreamPendingLimit {
			dropped := len(stream.pending) - appStreamPendingLimit
			stream.pending = append([]byte(nil), stream.pending[len(stream.pending)-appStreamPendingLimit:]...)
			a.recordDebugPendingDropped(sessionID, dropped)
		}
		return
	}
	value := a.buffers[sessionID] + string(data)
	if len(value) > appStreamBufferLimit {
		value = value[len(value)-appStreamBufferLimit:]
	}
	a.buffers[sessionID] = value
}

func (a *App) processPendingStreamOutput(limit int) bool {
	if limit <= 0 {
		return false
	}
	a.ensureAppState()
	changed := false
	remaining := limit
	for _, stream := range a.streams {
		if stream == nil || len(stream.pending) == 0 || remaining <= 0 {
			continue
		}
		a.resizeStreamView(stream)
		if stream.view == nil {
			size := a.streamSizeFor(stream.sessionID)
			stream.view = terminalview.New(size.Cols, size.Rows)
			stream.size = size
		}
		n := min(len(stream.pending), remaining)
		stream.view.Write(stream.pending[:n])
		if !stream.visibleOutput && terminalLinesHaveVisibleContent(terminalScreenLines(stream.view.Screen())) {
			stream.visibleOutput = true
			a.debugf("stream visible output session=%q processed=%d", stream.sessionID, n)
		}
		if n == len(stream.pending) {
			stream.pending = nil
		} else {
			stream.pending = stream.pending[n:]
		}
		a.recordDebugPendingProcessed(stream.sessionID, n, len(stream.pending))
		a.debugf("stream pending processed session=%q bytes=%d remaining=%d", stream.sessionID, n, len(stream.pending))
		stream.viewDirty = true
		remaining -= n
		if !stream.resizeSettling {
			changed = true
		}
	}
	return changed
}

func (a *App) resizeStreamView(stream *appSessionStream) {
	if stream == nil || stream.view == nil {
		return
	}
	a.resizeStreamViewTo(stream, a.streamSizeFor(stream.sessionID))
}

func (a *App) resizeStreamViewTo(stream *appSessionStream, size protocol.TerminalSize) {
	if stream == nil || stream.view == nil {
		return
	}
	if size.Cols < 1 {
		size.Cols = 80
	}
	if size.Rows < 1 {
		size.Rows = 24
	}
	if stream.size == size {
		return
	}
	stream.view.Resize(size.Cols, size.Rows)
	stream.size = size
	stream.viewDirty = true
	a.recordDebugResize(stream.sessionID, size)
	a.scheduleStreamResize(stream, size, time.Now())
}

func (a *App) scheduleStreamResize(stream *appSessionStream, size protocol.TerminalSize, now time.Time) {
	if stream == nil {
		return
	}
	stream.pendingResize = size
	stream.resizePending = true
	stream.resizeDue = now.Add(appStreamResizeDebounce)
	a.debugf("resize scheduled session=%q cols=%d rows=%d due=%s", stream.sessionID, size.Cols, size.Rows, stream.resizeDue.Format(time.RFC3339Nano))
}

func (a *App) delayPendingStreamResize(stream *appSessionStream, now time.Time) {
	if stream == nil || !stream.resizePending {
		return
	}
	stream.resizeDue = now.Add(appStreamResizeDebounce)
	a.debugf("resize delayed session=%q cols=%d rows=%d due=%s", stream.sessionID, stream.pendingResize.Cols, stream.pendingResize.Rows, stream.resizeDue.Format(time.RFC3339Nano))
}

func (a *App) flushDueStreamResizes(now time.Time) bool {
	if a == nil || a.dragSplit {
		return false
	}
	a.ensureAppState()
	for _, stream := range a.streams {
		if stream == nil || stream.stream == nil || !stream.resizePending || now.Before(stream.resizeDue) {
			continue
		}
		size := stream.pendingResize
		if a.queueStreamResizeNow(stream, size, now) {
			stream.resizePending = false
			stream.pendingResize = protocol.TerminalSize{}
			stream.resizeDue = time.Time{}
		} else {
			stream.resizeDue = now.Add(appStreamResizeDebounce)
		}
	}
	return false
}

func (a *App) queueStreamResizeNow(stream *appSessionStream, size protocol.TerminalSize, now time.Time) bool {
	if stream == nil || stream.stream == nil {
		return false
	}
	if stream.writes == nil {
		if err := stream.stream.Resize(size); err != nil {
			a.debugf("resize failed session=%q cols=%d rows=%d error=%q", stream.sessionID, size.Cols, size.Rows, err.Error())
			return false
		}
		a.startResizeSettle(stream, now)
		return true
	}
	select {
	case stream.writes <- appStreamWrite{size: size, resize: true}:
		a.beginResizeSettle(stream, now)
		return true
	default:
		a.debugf("resize dropped session=%q cols=%d rows=%d", stream.sessionID, size.Cols, size.Rows)
		return false
	}
}

func (a *App) beginResizeSettle(stream *appSessionStream, now time.Time) {
	if stream == nil {
		return
	}
	stream.resizeSettling = true
	stream.resizeAwaitingWrite = true
	stream.resizeSawOutput = false
	stream.resizeQuiet = time.Time{}
	stream.resizeMax = now.Add(appStreamResizeSettleMax)
	a.debugf("resize settle pending session=%q max=%s", stream.sessionID, stream.resizeMax.Format(time.RFC3339Nano))
}

func (a *App) startResizeSettle(stream *appSessionStream, now time.Time) {
	if stream == nil {
		return
	}
	sawOutput := stream.resizeSawOutput
	stream.resizeSettling = true
	stream.resizeAwaitingWrite = false
	stream.resizeSawOutput = sawOutput
	stream.resizeQuiet = now.Add(appStreamResizeSettleQuiet)
	stream.resizeMax = now.Add(appStreamResizeSettleMax)
	a.debugf("resize settle start session=%q quiet=%s max=%s", stream.sessionID, stream.resizeQuiet.Format(time.RFC3339Nano), stream.resizeMax.Format(time.RFC3339Nano))
}

func (a *App) finishResizeSettles(now time.Time) bool {
	if a == nil {
		return false
	}
	a.ensureAppState()
	changed := false
	for _, stream := range a.streams {
		if stream == nil || !stream.resizeSettling {
			continue
		}
		if len(stream.pending) > 0 {
			continue
		}
		if stream.resizeAwaitingWrite {
			if now.Before(stream.resizeMax) {
				continue
			}
		} else if stream.resizeSawOutput {
			if now.Before(stream.resizeMax) && now.Before(stream.resizeQuiet) {
				continue
			}
		} else if now.Before(stream.resizeMax) {
			continue
		}
		stream.resizeSettling = false
		stream.resizeAwaitingWrite = false
		stream.resizeSawOutput = false
		stream.resizeQuiet = time.Time{}
		stream.resizeMax = time.Time{}
		stream.viewDirty = true
		a.debugf("resize settle complete session=%q pending=%d", stream.sessionID, len(stream.pending))
		changed = true
	}
	return changed
}

func (a *App) refreshDirtyTerminalViews() bool {
	a.ensureAppState()
	changed := false
	for _, stream := range a.streams {
		if stream == nil || !stream.viewDirty || stream.resizeSettling {
			continue
		}
		a.updateStreamBuffer(stream)
		changed = true
	}
	return changed
}

func (a *App) updateStreamBuffer(stream *appSessionStream) {
	if stream == nil || stream.view == nil || stream.sessionID == "" {
		return
	}
	a.buffers[stream.sessionID] = stream.view.Render()
	stream.viewDirty = false
	if !stream.warming && a.selectedSession().ID == stream.sessionID {
		a.loadSelectedPreview()
	}
}

func (a *App) finishWarmStreams(now time.Time) bool {
	a.ensureAppState()
	changed := false
	for sessionID, stream := range a.streams {
		if stream == nil || !stream.warming {
			continue
		}
		if now.Before(stream.maxWarm) && (now.Before(stream.warmUntil) || now.Before(stream.quietUntil)) {
			continue
		}
		stream.warming = false
		if a.selectedSession().ID == sessionID {
			a.loadSelectedPreview()
			changed = true
		}
		if a.active == sessionID {
			if a.fullscreen {
				a.status = attachedFullscreenStatus(sessionID)
			} else {
				a.status = attachedPreviewStatus(sessionID)
			}
			changed = true
		}
	}
	return changed
}

func (a *App) cleanupStreams(now time.Time) bool {
	a.ensureAppState()
	changed := false
	for sessionID, stream := range a.streams {
		if stream == nil || sessionID == a.active || stream.keepUntil.IsZero() || now.Before(stream.keepUntil) {
			continue
		}
		a.closeSessionStream(sessionID)
		changed = true
	}
	return changed
}

func (a *App) closeSessionStream(sessionID string) {
	stream := a.streams[sessionID]
	if stream == nil {
		return
	}
	stream.closing = true
	delete(a.streams, sessionID)
	a.debugf("stream close requested session=%q", sessionID)
	if stream.cancel != nil {
		stream.cancel()
	}
	if stream.stream != nil {
		go func(stream *Stream) {
			_ = stream.Close()
		}(stream.stream)
	}
	if stream.view != nil {
		stream.view.Close()
	}
}

func (a *App) closeAllStreams() {
	a.ensureAppState()
	sessionIDs := make([]string, 0, len(a.streams))
	for sessionID := range a.streams {
		sessionIDs = append(sessionIDs, sessionID)
	}
	for _, sessionID := range sessionIDs {
		a.closeSessionStream(sessionID)
	}
	a.active = ""
	a.fullscreen = false
}

func (a *App) streamSize() protocol.TerminalSize {
	return a.streamSizeFor("")
}

func (a *App) streamSizeFor(sessionID string) protocol.TerminalSize {
	cols, rows := 120, 36
	if a.In != nil {
		if c, r, err := term.Size(a.In); err == nil {
			cols, rows = c, r
		}
	}
	if a.fullscreen && a.active != "" && (sessionID == "" || sessionID == a.active) {
		return appActiveStreamSize(cols, rows)
	}
	_, previewWidth, limit := a.layoutForSize(cols, rows)
	if previewWidth <= 0 {
		previewWidth = cols
	}
	if limit <= 0 {
		limit = rows
	}
	return protocol.TerminalSize{Cols: previewWidth, Rows: limit}
}

func appActiveStreamSize(cols, rows int) protocol.TerminalSize {
	if rows < 1 {
		rows = 24
	}
	if cols < 1 {
		cols = 80
	}
	return protocol.TerminalSize{Cols: cols, Rows: rows}
}

func (a *App) promptSlash(ctx context.Context) {
	value := a.promptInline("/")
	switch strings.TrimSpace(value) {
	case "login":
		a.promptLogin(ctx)
	case "refresh", "r":
		a.refresh(ctx)
	case "fullscreen", "fs":
		if err := a.attach(ctx); err != nil {
			a.err = err
			a.status = "attach failed"
			return
		}
		a.enterFullscreen()
	case "help", "?":
		a.status = "commands: /login /refresh /fullscreen /help"
	case "":
		a.status = "command canceled"
	default:
		a.status = "unknown command /" + strings.TrimSpace(value)
	}
}

func (a *App) promptLogin(ctx context.Context) {
	defaultHub := a.Client.HubURL
	if defaultHub == "" {
		defaultHub = "http://127.0.0.1:8080"
	}
	hubURL := a.promptInline("hub", defaultHub)
	hubURL = strings.TrimSpace(hubURL)
	if hubURL == "" {
		a.status = "login canceled"
		return
	}
	a.status = "starting login"
	a.render()
	auth, err := DeviceLogin(ctx, hubURL, "", "", func(start DeviceStartResponse) {
		a.status = "open " + start.VerificationURLComplete + "  code " + start.UserCode
		a.err = nil
		a.render()
	})
	if err != nil {
		a.err = err
		a.status = "login failed"
		return
	}
	a.Auth = auth
	a.Client = auth.Client
	a.loggedIn = true
	a.err = nil
	a.status = "login complete"
	a.refresh(ctx)
}

func (a *App) ensureLoggedIn() bool {
	if a.loggedIn && a.Client.HubURL != "" && a.Client.Token != "" {
		return true
	}
	a.loggedIn = false
	a.status = "not logged in; type /login"
	return false
}

func (a *App) render() {
	cols, rows, err := term.Size(a.In)
	if err != nil {
		cols, rows = 120, 36
	}
	a.renderWithSize(cols, rows)
}

func (a *App) renderWithSize(cols, rows int) {
	a.recordDebugRender(cols, rows)
	moveCursor(a.Out, 1, 1)
	hideCursor(a.Out)
	defer showCursor(a.Out)
	if a.fullscreen && a.active != "" {
		a.renderFullscreenWithSize(cols, rows)
		return
	}
	source := a.Auth.Source
	if source == "" {
		if a.loggedIn {
			source = "unknown"
		} else {
			source = "none"
		}
	}
	meta := fmt.Sprintf("%s  hub=%s  auth=%s", styleTitle("AgentMux"), a.Client.HubURL, source)
	if a.Auth.TenantID != "" {
		meta += "  tenant=" + a.Auth.TenantID
	}
	writeLine(a.Out, cols, meta)
	workerSummary := styleHeader("Workers") + ": "
	if len(a.workers) == 0 {
		workerSummary += styleMuted("none")
	} else {
		parts := make([]string, 0, len(a.workers))
		for _, worker := range a.workers {
			parts = append(parts, workerStatusMarker(worker)+" "+worker.ID+styleMuted("("+worker.Name+")"))
		}
		workerSummary += strings.Join(parts, ", ")
	}
	writeLine(a.Out, cols, workerSummary)
	listWidth, previewWidth, limit := a.layoutForSize(cols, rows)
	writeLine(a.Out, cols, styleHeader("Sessions")+previewTitle(listWidth, previewWidth, a.active))
	start := scrollStart(a.selected, limit, len(a.sessions))
	previewLines := splitPreviewLines(a.preview, limit)
	bodyStart := 4
	if previewWidth > 0 {
		a.renderSplitRows(bodyStart, limit, listWidth, previewWidth, start, previewLines)
		moveCursor(a.Out, bodyStart+limit, 1)
	} else {
		for row := 0; row < limit; row++ {
			sessionIndex := start + row
			left := a.sessionListLine(row, sessionIndex, false)
			writeLine(a.Out, cols, left)
		}
	}
	selected := a.selectedSession()
	footer := styleMuted("Status: ") + a.status
	if selected.ID != "" {
		footer += styleMuted("  |  ") + styleHeader("Selected ") + selected.ID
	}
	if a.active != "" {
		footer += styleMuted("  |  ") + styleOK("Input ") + a.active
	}
	if a.err != nil {
		footer += styleMuted("  |  ") + styleError(a.err.Error())
	}
	if a.debugEnabled() {
		footer += styleMuted("  |  ") + a.debugHUD()
	}
	writeLine(a.Out, cols, footer)
	if a.active != "" {
		if a.debugEnabled() {
			writeLine(a.Out, cols, styleMuted("Ctrl-] detach  Ctrl-F fullscreen  Ctrl-G debug  mouse select/drag split"))
		} else {
			writeLine(a.Out, cols, styleMuted("Ctrl-] detach  Ctrl-F fullscreen  mouse select/drag split"))
		}
	} else if a.debugEnabled() {
		writeLine(a.Out, cols, styleMuted("Enter/a attach  mouse select/drag split  / commands  c create  s send  x stop  r refresh  D debug  ? help  q quit"))
	} else {
		writeLine(a.Out, cols, styleMuted("Enter/a attach  mouse select/drag split  / commands  c create  s send  x stop  r refresh  ? help  q quit"))
	}
	clearToEnd(a.Out)
}

func (a *App) renderFullscreenWithSize(cols, rows int) {
	size := appActiveStreamSize(cols, rows)
	stream := a.streams[a.active]
	if stream != nil {
		a.resizeStreamViewTo(stream, size)
	}
	lines := []string(nil)
	cursorX, cursorY, cursorOK := 0, 0, false
	if stream != nil && stream.view != nil {
		lines = terminalScreenLines(stream.view.Screen())
		cursorX, cursorY, cursorOK = stream.view.Cursor()
	}
	if stream == nil || !stream.seenOutput {
		if fallback := a.activeFallbackLines(stream, size); len(fallback) > 0 {
			lines = fallback
			cursorOK = false
		}
	}
	for row := 0; row < size.Rows; row++ {
		line := ""
		if row < len(lines) {
			line = lines[row]
		}
		writeCanvasLine(a.Out, row+1, cols, line)
	}
	if cursorOK {
		if cursorX < 0 {
			cursorX = 0
		}
		if cursorY < 0 {
			cursorY = 0
		}
		if cursorX >= cols {
			cursorX = cols - 1
		}
		if cursorY >= size.Rows {
			cursorY = size.Rows - 1
		}
		moveCursor(a.Out, cursorY+1, cursorX+1)
	}
}

func (a *App) activeFallbackLines(stream *appSessionStream, size protocol.TerminalSize) []string {
	if a.active == "" {
		return nil
	}
	if stream != nil && stream.seenOutput {
		return nil
	}
	status := "attached " + a.active + "  waiting for terminal output  Ctrl-F split  Ctrl-]/Esc detach  q/Ctrl-C/Ctrl-Q quit"
	if stream == nil || stream.connecting || stream.stream == nil {
		status = "connecting " + a.active + "  Ctrl-F split  Ctrl-]/Esc detach  q/Ctrl-C/Ctrl-Q quit"
	}
	if a.debugEnabled() {
		status += "  Ctrl-G debug"
	}
	lines := []string(nil)
	if capture := a.activeFallbackCapture(); capture != "" {
		raw := splitPreviewLines(capture, size.Rows)
		lines = make([]string, 0, len(raw))
		for i := range raw {
			lines = append(lines, previewLine(raw, i))
		}
	}
	if len(lines) == 0 {
		lines = []string{styleMuted(status)}
	} else if size.Rows > 0 {
		if len(lines) < size.Rows {
			for len(lines) < size.Rows-1 {
				lines = append(lines, "")
			}
			lines = append(lines, styleMuted(status))
		} else {
			lines[size.Rows-1] = styleMuted(status)
		}
	}
	return lines
}

func (a *App) activeFallbackCapture() string {
	if a == nil || a.active == "" {
		return ""
	}
	if a.buffers != nil && a.buffers[a.active] != "" {
		return a.buffers[a.active]
	}
	if a.previews != nil {
		return a.previews[a.active]
	}
	return ""
}

func (a *App) activeWaitingForStreamOutput() bool {
	if a == nil || a.active == "" {
		return false
	}
	stream := a.streams[a.active]
	if stream == nil {
		return true
	}
	if stream.seenOutput {
		return false
	}
	return true
}

func (a *App) renderSplitRows(bodyStart, limit, listWidth, previewWidth, start int, previewLines []string) {
	for row := 0; row < limit; row++ {
		screenRow := bodyStart + row
		sessionIndex := start + row
		left := a.sessionListLine(row, sessionIndex, true)
		right := previewLine(previewLines, row)
		if len(a.preview) == 0 && row == 0 && a.selectedStreamWarming() {
			right = styleMuted("stream warming")
		}
		writeAt(a.Out, screenRow, listWidth+4, padVisible(truncateVisible(right, previewWidth), previewWidth))
		writeAt(a.Out, screenRow, listWidth+1, " | ")
		writeAt(a.Out, screenRow, 1, padVisible(left, listWidth))
	}
}

func terminalScreenLines(screen string) []string {
	if screen == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(screen, "\n"), "\n")
}

func terminalLinesHaveVisibleContent(lines []string) bool {
	for _, line := range lines {
		if strings.TrimSpace(stripControl(line)) != "" {
			return true
		}
	}
	return false
}

func (a *App) selectedStreamWarming() bool {
	session := a.selectedSession()
	if session.ID == "" || a.streams == nil {
		return false
	}
	stream := a.streams[session.ID]
	return stream != nil && stream.warming
}

func (a *App) sessionListLine(row, sessionIndex int, compact bool) string {
	if len(a.sessions) == 0 && row == 0 {
		return "  no sessions"
	}
	if sessionIndex >= len(a.sessions) {
		return ""
	}
	session := a.sessions[sessionIndex]
	prefix := "  "
	if sessionIndex == a.selected {
		prefix = "> "
	}
	if session.ID == a.active {
		prefix = styleOK("* ")
	}
	if compact {
		return compactSessionLine(prefix, session)
	}
	return fullSessionLine(prefix, session, styleStatus(session.Status))
}

func (a *App) promptLine(label, fallback string) string {
	if fallback == "" {
		fmt.Fprintf(a.Out, "%s: ", label)
	} else {
		fmt.Fprintf(a.Out, "%s [%s]: ", label, fallback)
	}
	reader := bufio.NewReader(a.In)
	value, _ := reader.ReadString('\n')
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}

func (a *App) promptInline(label string, fallback ...string) string {
	defaultValue := ""
	if len(fallback) > 0 {
		defaultValue = fallback[0]
	}
	cols, rows, err := term.Size(a.In)
	if err != nil {
		cols, rows = 120, 36
	}
	prompt := label + " "
	if defaultValue != "" {
		prompt += "[" + defaultValue + "] "
	}
	moveCursor(a.Out, rows, 1)
	_, _ = io.WriteString(a.Out, "\x1b[2K"+styleAccent(prompt))
	var value strings.Builder
	for {
		key, err := a.keys.Read(a.In)
		if err != nil {
			a.err = err
			return ""
		}
		switch key {
		case "enter":
			if strings.TrimSpace(value.String()) == "" {
				return defaultValue
			}
			return value.String()
		case "unknown":
			if value.Len() == 0 {
				return ""
			}
		default:
			if len(key) == 1 {
				b := key[0]
				switch b {
				case 0x7f, '\b':
					text := value.String()
					if len(text) > 0 {
						value.Reset()
						value.WriteString(text[:len(text)-1])
					}
				default:
					if b >= 0x20 {
						value.WriteByte(b)
					}
				}
			}
		}
		moveCursor(a.Out, rows, 1)
		line := styleAccent(prompt) + value.String()
		_, _ = io.WriteString(a.Out, "\x1b[2K"+truncateVisible(line, cols-1))
	}
}

func (a *App) leaveRawForPrompt() {
	a.disableRaw()
	term.ResetModes(a.Out)
	term.ExitAlternateScreen(a.Out)
	_, _ = io.WriteString(a.Out, "\n")
}

func (a *App) enterRawAfterPrompt() {
	term.EnterAlternateScreen(a.Out)
	term.EnableMouseCellMotion(a.Out)
	_ = a.enableRaw()
}

func (a *App) enableRaw() error {
	if a.rawRestore != nil {
		return nil
	}
	restore, err := term.MakeRawWithTimeout(a.In, 0, 1)
	if err != nil {
		return err
	}
	a.rawRestore = restore
	return nil
}

func (a *App) disableRaw() {
	if a.rawRestore == nil {
		return
	}
	a.rawRestore()
	a.rawRestore = nil
}

func (a *App) selectedSession() protocol.SessionView {
	if a.selected < 0 || a.selected >= len(a.sessions) {
		return protocol.SessionView{}
	}
	return a.sessions[a.selected]
}

func (a *App) clampSelection() {
	if len(a.sessions) == 0 {
		a.selected = 0
		return
	}
	if a.selected < 0 {
		a.selected = 0
	}
	if a.selected >= len(a.sessions) {
		a.selected = len(a.sessions) - 1
	}
}

func readAppKey(in io.Reader) (string, error) {
	var reader appKeyReader
	return reader.Read(in)
}

type appKeyReader struct {
	pending []byte
	utf8Buf []byte
}

func (r *appKeyReader) Read(in io.Reader) (string, error) {
	for {
		input, ok, err := r.ReadEventAvailable(in)
		if err != nil || ok {
			return input.key, err
		}
	}
}

func (r *appKeyReader) ReadAvailable(in io.Reader) (string, bool, error) {
	input, ok, err := r.ReadEventAvailable(in)
	if !ok || err != nil {
		return "", ok, err
	}
	if input.mouse != nil {
		return "mouse", true, nil
	}
	return input.key, true, nil
}

func (r *appKeyReader) ReadEventAvailable(in io.Reader) (appInputEvent, bool, error) {
	if len(r.pending) > 0 {
		b := r.pending[0]
		r.pending = r.pending[1:]
		key, ok, err := r.keyFromInputByte(b)
		return appInputEvent{key: key}, ok, err
	}
	var b [1]byte
	n, err := in.Read(b[:])
	if n == 0 {
		if err != nil && !errors.Is(err, io.EOF) {
			return appInputEvent{}, false, err
		}
		return appInputEvent{}, false, nil
	}
	switch b[0] {
	case 0x1b:
		r.utf8Buf = nil
		seq := make([]byte, 0, 32)
		var next [1]byte
		for len(seq) < 32 {
			n, err := in.Read(next[:])
			if n > 0 {
				seq = append(seq, next[0])
				if appEscapeComplete(seq) {
					break
				}
				continue
			}
			if err != nil && !errors.Is(err, io.EOF) {
				return appInputEvent{}, false, err
			}
			break
		}
		if mouse, ok := parseAppMouseSequence(seq); ok {
			return appInputEvent{mouse: &mouse}, true, nil
		}
		if len(seq) >= 2 && (seq[0] == '[' || seq[0] == 'O') {
			switch seq[1] {
			case 'A':
				return appInputEvent{key: "up"}, true, nil
			case 'B':
				return appInputEvent{key: "down"}, true, nil
			case 'C':
				return appInputEvent{key: "right"}, true, nil
			case 'D':
				return appInputEvent{key: "left"}, true, nil
			}
		}
		if len(seq) == 3 && seq[0] == '[' && seq[1] == '3' && seq[2] == '~' {
			return appInputEvent{key: "delete"}, true, nil
		}
		if len(seq) > 0 {
			r.pending = append(r.pending, seq...)
		}
		return appInputEvent{key: "unknown"}, true, nil
	default:
		key, ok, err := r.keyFromInputByte(b[0])
		return appInputEvent{key: key}, ok, err
	}
}

func (r *appKeyReader) keyFromInputByte(b byte) (string, bool, error) {
	if len(r.utf8Buf) == 0 && b < utf8.RuneSelf {
		return keyFromByte(b), true, nil
	}
	r.utf8Buf = append(r.utf8Buf, b)
	if !utf8.FullRune(r.utf8Buf) {
		return "", false, nil
	}
	if !utf8.Valid(r.utf8Buf) {
		first := r.utf8Buf[0]
		if len(r.utf8Buf) > 1 {
			r.pending = append(append([]byte(nil), r.utf8Buf[1:]...), r.pending...)
		}
		r.utf8Buf = nil
		return keyFromByte(first), true, nil
	}
	key := string(r.utf8Buf)
	r.utf8Buf = nil
	return key, true, nil
}

func appEscapeComplete(seq []byte) bool {
	if len(seq) == 0 {
		return false
	}
	switch seq[0] {
	case '[':
		if len(seq) < 2 {
			return false
		}
		last := seq[len(seq)-1]
		return last >= 0x40 && last <= 0x7e
	case 'O':
		return len(seq) >= 2
	default:
		return true
	}
}

func parseAppMouseSequence(seq []byte) (appMouseEvent, bool) {
	if len(seq) < 6 || seq[0] != '[' || seq[1] != '<' {
		return appMouseEvent{}, false
	}
	final := seq[len(seq)-1]
	if final != 'M' && final != 'm' {
		return appMouseEvent{}, false
	}
	parts := strings.Split(string(seq[2:len(seq)-1]), ";")
	if len(parts) != 3 {
		return appMouseEvent{}, false
	}
	code, err := strconv.Atoi(parts[0])
	if err != nil {
		return appMouseEvent{}, false
	}
	x, err := strconv.Atoi(parts[1])
	if err != nil {
		return appMouseEvent{}, false
	}
	y, err := strconv.Atoi(parts[2])
	if err != nil {
		return appMouseEvent{}, false
	}
	event := appMouseEvent{
		x:      x,
		y:      y,
		shift:  code&4 != 0,
		alt:    code&8 != 0,
		ctrl:   code&16 != 0,
		button: terminalview.MouseNone,
	}
	release := final == 'm'
	buttonCode := code & 3
	switch {
	case code&64 != 0:
		event.kind = appMouseWheel
		if buttonCode == 0 {
			event.button = terminalview.MouseWheelUp
		} else if buttonCode == 1 {
			event.button = terminalview.MouseWheelDown
		} else if buttonCode == 2 {
			event.button = terminalview.MouseWheelLeft
		} else {
			event.button = terminalview.MouseWheelRight
		}
	case release:
		event.kind = appMouseRelease
	case code&32 != 0:
		event.kind = appMouseMotion
	default:
		event.kind = appMouseClick
	}
	if event.kind == appMouseClick || event.kind == appMouseMotion {
		switch buttonCode {
		case 0:
			event.button = terminalview.MouseLeft
		case 1:
			event.button = terminalview.MouseMiddle
		case 2:
			event.button = terminalview.MouseRight
		default:
			event.button = terminalview.MouseNone
		}
	}
	return event, true
}

func (r *appKeyReader) Reset() {
	r.pending = nil
	r.utf8Buf = nil
}

func keyFromByte(b byte) string {
	switch b {
	case '\r', '\n':
		return "enter"
	case 0x03:
		return "ctrl-c"
	case 0x06:
		return "ctrl-f"
	case 0x11:
		return "ctrl-q"
	case 0x07:
		return "ctrl-g"
	case 0x1d:
		return "detach"
	case '\t':
		return "tab"
	case 0x7f, '\b':
		return string([]byte{b})
	default:
		return string([]byte{b})
	}
}

func attachedPreviewStatus(sessionID string) string {
	return "attached " + sessionID + " in preview  Ctrl-F fullscreen  Ctrl-] detach"
}

func attachedFullscreenStatus(sessionID string) string {
	return "fullscreen " + sessionID + "  Ctrl-F split  Ctrl-] detach"
}

func clear(out io.Writer) {
	_, _ = io.WriteString(out, "\x1b[H\x1b[2J")
}

func clearLine(out io.Writer) {
	_, _ = io.WriteString(out, "\x1b[2K")
}

func clearToEnd(out io.Writer) {
	_, _ = io.WriteString(out, "\x1b[J")
}

func hideCursor(out io.Writer) {
	_, _ = io.WriteString(out, "\x1b[?25l")
}

func showCursor(out io.Writer) {
	_, _ = io.WriteString(out, "\x1b[?25h")
}

func moveCursor(out io.Writer, row, col int) {
	if row < 1 {
		row = 1
	}
	if col < 1 {
		col = 1
	}
	_, _ = fmt.Fprintf(out, "\x1b[%d;%dH", row, col)
}

func writeAt(out io.Writer, row, col int, value string) {
	moveCursor(out, row, col)
	_, _ = io.WriteString(out, value)
}

func writeLine(out io.Writer, cols int, line string) {
	clearLine(out)
	if cols > 1 && visibleLen(line) > cols-1 {
		line = truncateVisible(line, cols-1)
	}
	_, _ = io.WriteString(out, line+"\r\n")
}

func writeCanvasLine(out io.Writer, row, cols int, line string) {
	moveCursor(out, row, 1)
	clearLine(out)
	if cols > 0 && visibleLen(line) > cols {
		line = truncateVisible(line, cols)
	}
	_, _ = io.WriteString(out, line+"\x1b[0m")
}

func appLayout(cols, rows int) (listWidth, previewWidth, limit int) {
	listWidth = cols
	previewWidth = 0
	if cols >= 72 {
		listWidth = min(max(24, cols/4), 34)
		previewWidth = cols - listWidth - 3
	}
	limit = rows - 7
	if limit < 4 {
		limit = 4
	}
	return listWidth, previewWidth, limit
}

func (a *App) layoutForSize(cols, rows int) (listWidth, previewWidth, limit int) {
	listWidth, previewWidth, limit = appLayout(cols, rows)
	if previewWidth > 0 && a != nil && a.splitWidth > 0 {
		minList := 18
		maxList := cols - 32 - 3
		if maxList < minList {
			maxList = minList
		}
		listWidth = clampInt(a.splitWidth, minList, maxList)
		previewWidth = cols - listWidth - 3
	}
	return listWidth, previewWidth, limit
}

func (a *App) setSplitWidth(width, cols int) {
	if cols < 72 {
		a.splitWidth = 0
		return
	}
	minList := 18
	maxList := cols - 32 - 3
	if maxList < minList {
		maxList = minList
	}
	a.splitWidth = clampInt(width, minList, maxList)
}

func clampInt(value, low, high int) int {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

func previewTitle(listWidth, previewWidth int, active string) string {
	if previewWidth <= 0 {
		return ""
	}
	title := styleHeader("Preview")
	if active != "" {
		title = styleOK("Session " + active)
	}
	return strings.Repeat(" ", max(0, listWidth-len("Sessions"))) + " | " + title
}

func compactSessionLine(prefix string, session protocol.SessionView) string {
	command := session.Command
	if command == "" {
		command = "-"
	}
	status := session.Status
	if status == "" {
		status = "unknown"
	}
	line := prefix +
		padPlain(ellipsizePlain(session.ID, 16), 17) +
		padPlain(ellipsizePlain(status, 7), 8) +
		ellipsizePlain(command, 6)
	return line
}

func padPlain(value string, width int) string {
	if width <= 0 {
		return value
	}
	value = stripControl(value)
	if len(value) > width {
		return ellipsizePlain(value, width)
	}
	if len(value) == width {
		return value
	}
	return value + strings.Repeat(" ", width-len(value))
}

func ellipsizePlain(value string, limit int) string {
	value = stripControl(value)
	if limit <= 0 {
		return ""
	}
	if len(value) <= limit {
		return value
	}
	if limit <= 1 {
		return value[:limit]
	}
	return value[:limit-1] + "~"
}

func fullSessionLine(prefix string, session protocol.SessionView, status string) string {
	return prefix +
		padVisible(ellipsize(session.ID, 28), 29) +
		padVisible(status, 14) +
		padVisible(ellipsize(session.Command, 18), 19) +
		styleMuted(session.CWD)
}

func splitPreviewLines(preview string, limit int) []string {
	raw := strings.Split(strings.TrimRight(preview, "\n"), "\n")
	if len(raw) == 1 && raw[0] == "" {
		return nil
	}
	if len(raw) > limit {
		raw = raw[len(raw)-limit:]
	}
	return raw
}

func previewLine(lines []string, index int) string {
	if index < 0 || index >= len(lines) {
		return ""
	}
	line := strings.ReplaceAll(lines[index], "\t", "    ")
	return highlightPreviewLine(sanitizePreviewANSI(line))
}

func stripControl(value string) string {
	var out strings.Builder
	for i := 0; i < len(value); i++ {
		b := value[i]
		if b == 0x1b {
			i = skipEscape(value, i)
			continue
		}
		if b < 0x20 && b != '\t' {
			continue
		}
		out.WriteByte(b)
	}
	return out.String()
}

func sanitizePreviewANSI(value string) string {
	var out strings.Builder
	for i := 0; i < len(value); i++ {
		b := value[i]
		if b == 0x1b {
			end := skipEscape(value, i)
			if isSGR(value, i, end) {
				out.WriteString(value[i : end+1])
			}
			i = end
			continue
		}
		if b < 0x20 && b != '\t' {
			continue
		}
		out.WriteByte(b)
	}
	out.WriteString("\x1b[0m")
	return out.String()
}

func highlightPreviewLine(value string) string {
	if hasSGR(value) {
		return value
	}
	plain := stripControl(value)
	trimmed := strings.TrimSpace(plain)
	lower := strings.ToLower(trimmed)
	switch {
	case trimmed == "":
		return plain
	case strings.Contains(lower, "error") || strings.Contains(lower, "failed") || strings.Contains(lower, "panic"):
		return styleError(plain)
	case strings.Contains(lower, "warning") || strings.Contains(lower, "warn"):
		return styleWarn(plain)
	case strings.Contains(lower, "success") || strings.Contains(lower, "ready") || strings.Contains(lower, "done"):
		return styleOK(plain)
	case strings.HasPrefix(trimmed, "$") || strings.HasPrefix(trimmed, ">") || strings.HasSuffix(trimmed, "%"):
		return styleAccent(plain)
	case strings.Contains(trimmed, "/") && !strings.Contains(trimmed, " "):
		return styleMuted(plain)
	default:
		return plain
	}
}

func hasSGR(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] != 0x1b {
			continue
		}
		end := skipEscape(value, i)
		if isSGR(value, i, end) {
			return true
		}
		i = end
	}
	return false
}

func padVisible(value string, width int) string {
	if width <= 0 {
		return value
	}
	n := visibleLen(value)
	if n >= width {
		return truncateVisible(value, width)
	}
	return value + strings.Repeat(" ", width-n)
}

func ellipsize(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if visibleLen(value) <= limit {
		return value
	}
	if limit <= 1 {
		return truncateVisible(value, limit)
	}
	return truncateVisible(value, limit-1) + "~"
}

func scrollStart(selected, limit, total int) int {
	if total <= limit {
		return 0
	}
	start := selected - limit/2
	if start < 0 {
		return 0
	}
	if start+limit > total {
		return total - limit
	}
	return start
}

func defaultWorkerID(workers []protocol.WorkerView) string {
	if len(workers) == 0 {
		return ""
	}
	for _, worker := range workers {
		if workerIsOnline(worker) {
			return worker.ID
		}
	}
	return workers[0].ID
}

func workerIsOnline(worker protocol.WorkerView) bool {
	return worker.Online || worker.Status == "" || worker.Status == "online"
}

func workerStatusMarker(worker protocol.WorkerView) string {
	if workerIsOnline(worker) {
		return styleOK("*")
	}
	return styleWarn("!")
}

func dedupeWorkers(workers []protocol.WorkerView) []protocol.WorkerView {
	byID := map[string]protocol.WorkerView{}
	for _, worker := range workers {
		if worker.ID == "" {
			continue
		}
		existing, ok := byID[worker.ID]
		if !ok || worker.LastSeen.After(existing.LastSeen) {
			byID[worker.ID] = worker
		}
	}
	result := make([]protocol.WorkerView, 0, len(byID))
	for _, worker := range byID {
		result = append(result, worker)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].ID < result[j].ID
	})
	return result
}

func dedupeSessions(sessions []protocol.SessionView) []protocol.SessionView {
	byID := map[string]protocol.SessionView{}
	for _, session := range sessions {
		if session.ID == "" {
			continue
		}
		byID[session.ID] = session
	}
	result := make([]protocol.SessionView, 0, len(byID))
	for _, session := range byID {
		result = append(result, session)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].ID < result[j].ID
	})
	return result
}

func styleTitle(value string) string {
	return "\x1b[1;36m" + value + "\x1b[0m"
}

func styleHeader(value string) string {
	return "\x1b[1;37m" + value + "\x1b[0m"
}

func styleAccent(value string) string {
	return "\x1b[36m" + value + "\x1b[0m"
}

func styleMuted(value string) string {
	return "\x1b[2m" + value + "\x1b[0m"
}

func styleOK(value string) string {
	return "\x1b[32m" + value + "\x1b[0m"
}

func styleError(value string) string {
	return "\x1b[31m" + value + "\x1b[0m"
}

func styleWarn(value string) string {
	return "\x1b[33m" + value + "\x1b[0m"
}

func styleStatus(status string) string {
	switch strings.ToLower(status) {
	case "active", "running":
		return styleOK("* " + status)
	case "stopped", "dead", "failed":
		return styleError("! " + status)
	default:
		return styleAccent("- " + status)
	}
}

func visibleLen(value string) int {
	n := 0
	for i := 0; i < len(value); {
		b := value[i]
		if b == 0x1b {
			i = skipEscape(value, i)
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(value[i:])
		if r == utf8.RuneError && size == 1 {
			n++
			i++
			continue
		}
		n += runewidth.RuneWidth(r)
		i += size
	}
	return n
}

func truncateVisible(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	var out strings.Builder
	visible := 0
	for i := 0; i < len(value); {
		b := value[i]
		if b == 0x1b {
			end := skipEscape(value, i)
			out.WriteString(value[i : end+1])
			i = end + 1
			continue
		}
		r, size := utf8.DecodeRuneInString(value[i:])
		if r == utf8.RuneError && size == 1 {
			if visible+1 > limit {
				break
			}
			out.WriteByte(value[i])
			visible++
			i++
			continue
		}
		width := runewidth.RuneWidth(r)
		if visible+width > limit {
			break
		}
		out.WriteString(value[i : i+size])
		visible += width
		i += size
	}
	out.WriteString("\x1b[0m")
	return out.String()
}

func skipEscape(value string, start int) int {
	if start+1 >= len(value) {
		return start
	}
	i := start + 1
	if value[i] == '[' {
		for i+1 < len(value) {
			i++
			if value[i] >= 0x40 && value[i] <= 0x7e {
				return i
			}
		}
		return i
	}
	return i
}

func isSGR(value string, start, end int) bool {
	return start+2 <= end && end < len(value) && value[start+1] == '[' && value[end] == 'm'
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
