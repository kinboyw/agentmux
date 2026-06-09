package control

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"private/agentmux/internal/protocol"
	"private/agentmux/internal/term"
)

type App struct {
	Client Client
	Auth   AppAuthResult
	In     *os.File
	Out    io.Writer

	workers  []protocol.WorkerView
	sessions []protocol.SessionView
	selected int
	status   string
	err      error
	preview  string
	previews map[string]string
	loggedIn bool
	streams  map[string]*appSessionStream
	buffers  map[string]string
	active   string
	events   chan appStreamEvent

	rawRestore func()
	keys       appKeyReader
}

const (
	appStreamBufferLimit = 256 * 1024
	appStreamDetachTTL   = 90 * time.Second
	appStreamWarmupMin   = 450 * time.Millisecond
	appStreamWarmupQuiet = 180 * time.Millisecond
	appStreamWarmupMax   = 1500 * time.Millisecond
	appRenderMinInterval = 50 * time.Millisecond
)

type appSessionStream struct {
	sessionID  string
	stream     *Stream
	cancel     context.CancelFunc
	connecting bool
	closing    bool
	keepUntil  time.Time
	warming    bool
	warmUntil  time.Time
	quietUntil time.Time
	maxWarm    time.Time
}

type appStreamEvent struct {
	sessionID string
	stream    *Stream
	connected bool
	data      []byte
	err       error
	closed    bool
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
		changed := a.drainStreamEvents(64)
		if time.Since(lastCleanup) >= time.Second {
			lastCleanup = time.Now()
			changed = a.cleanupStreams(time.Now()) || changed
		}
		changed = a.finishWarmStreams(time.Now()) || changed
		key, ok, err := a.keys.ReadAvailable(a.In)
		if err != nil {
			return err
		}
		if ok {
			quit := a.handleKey(ctx, key)
			a.clampSelection()
			changed = true
			if quit {
				return nil
			}
		}
		if changed {
			pendingRender = true
		}
		if pendingRender && (ok || time.Since(lastRender) >= appRenderMinInterval) {
			a.render()
			lastRender = time.Now()
			pendingRender = false
		}
	}
}

func (a *App) handleKey(ctx context.Context, key string) bool {
	if a.active != "" {
		if key == "detach" {
			a.detachActive()
			return false
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
			a.warmSelectedStream(ctx)
		}
	case "down", "j":
		if a.selected < len(a.sessions)-1 {
			a.selected++
			a.loadSelectedPreview()
			a.warmSelectedStream(ctx)
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
	case "?":
		a.status = "keys: up/down select, enter attach, Ctrl-] detach, / commands, c create, s send, x stop, r refresh, q quit"
	}
	return false
}

func (a *App) refresh(ctx context.Context) {
	if !a.ensureLoggedIn() {
		return
	}
	a.ensureAppState()
	workers, err := a.Client.Workers(ctx)
	if err != nil {
		a.err = err
		a.status = "refresh workers failed"
		return
	}
	sessions, err := a.Client.Sessions(ctx)
	if err != nil {
		a.err = err
		a.status = "refresh sessions failed"
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
	a.warmSelectedStream(ctx)
}

func (a *App) refreshPreviewCache(ctx context.Context) {
	a.previews = map[string]string{}
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
				data, err := a.Client.SessionPreview(ctx, session.ID, 80)
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

func (a *App) warmSelectedStream(ctx context.Context) {
	session := a.selectedSession()
	if session.ID == "" {
		return
	}
	_ = a.ensureStream(ctx, session.ID)
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
	a.status = "create queued"
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
	if err := a.ensureStream(ctx, session.ID); err != nil {
		return err
	}
	if stream := a.streams[session.ID]; stream != nil {
		stream.keepUntil = time.Time{}
	}
	a.active = session.ID
	a.status = "attached " + session.ID + "  Ctrl-] detach"
	a.loadSelectedPreview()
	return nil
}

func (a *App) detachActive() {
	if a.active == "" {
		return
	}
	sessionID := a.active
	a.active = ""
	if stream := a.streams[sessionID]; stream != nil {
		stream.keepUntil = time.Now().Add(appStreamDetachTTL)
	}
	a.status = "detached " + sessionID + "  stream kept " + appStreamDetachTTL.String()
	a.loadSelectedPreview()
}

func (a *App) forwardActiveKey(key string) error {
	sessionID := a.active
	if sessionID == "" {
		return nil
	}
	stream := a.streams[sessionID]
	if stream == nil || stream.stream == nil {
		a.status = "stream connecting " + sessionID
		return nil
	}
	data := appKeyInputData(key)
	if data == "" {
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
	default:
		if len(key) == 1 {
			return key
		}
		return ""
	}
}

func (a *App) ensureStream(ctx context.Context, sessionID string) error {
	if !a.ensureLoggedIn() {
		return nil
	}
	a.ensureAppState()
	if existing := a.streams[sessionID]; existing != nil {
		return nil
	}
	streamCtx, cancel := context.WithCancel(ctx)
	now := time.Now()
	state := &appSessionStream{
		sessionID: sessionID, cancel: cancel, connecting: true, warming: true,
		warmUntil: now.Add(appStreamWarmupMin), quietUntil: now.Add(appStreamWarmupQuiet), maxWarm: now.Add(appStreamWarmupMax),
	}
	a.streams[sessionID] = state
	size := a.streamSize()
	go a.openStream(streamCtx, sessionID, size)
	return nil
}

func (a *App) openStream(ctx context.Context, sessionID string, size protocol.TerminalSize) {
	stream, err := a.Client.OpenStream(ctx, sessionID, size)
	if err != nil {
		a.sendStreamEvent(appStreamEvent{sessionID: sessionID, err: err, closed: true})
		return
	}
	a.sendStreamEvent(appStreamEvent{sessionID: sessionID, stream: stream, connected: true})
	for {
		event, err := stream.ReadEvent()
		if len(event.Data) > 0 {
			a.sendStreamEvent(appStreamEvent{sessionID: sessionID, stream: stream, data: event.Data})
		}
		if event.Err != nil {
			a.sendStreamEvent(appStreamEvent{sessionID: sessionID, stream: stream, err: event.Err})
		}
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				a.sendStreamEvent(appStreamEvent{sessionID: sessionID, stream: stream, err: err, closed: true})
			} else {
				a.sendStreamEvent(appStreamEvent{sessionID: sessionID, stream: stream, closed: true})
			}
			return
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
			return
		}
		select {
		case <-events:
		default:
		}
		select {
		case events <- event:
		default:
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
			changed = true
		default:
			return changed
		}
	}
	return changed
}

func (a *App) applyStreamEvent(event appStreamEvent) {
	a.ensureAppState()
	stream := a.streams[event.sessionID]
	if stream == nil {
		if event.stream != nil {
			_ = event.stream.Close()
		}
		return
	}
	if event.connected {
		stream.stream = event.stream
		stream.connecting = false
		now := time.Now()
		stream.warming = true
		stream.warmUntil = now.Add(appStreamWarmupMin)
		stream.quietUntil = now.Add(appStreamWarmupQuiet)
		stream.maxWarm = now.Add(appStreamWarmupMax)
		if a.active == event.sessionID {
			a.status = "attached " + event.sessionID + "  warming stream"
		}
	}
	if len(event.data) > 0 {
		a.appendSessionBuffer(event.sessionID, string(event.data))
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
			if a.active == event.sessionID {
				a.active = ""
				a.status = "stream closed " + event.sessionID
			}
		}
	}
}

func (a *App) appendSessionBuffer(sessionID, data string) {
	if data == "" {
		return
	}
	a.ensureAppState()
	value := a.buffers[sessionID] + data
	if len(value) > appStreamBufferLimit {
		value = value[len(value)-appStreamBufferLimit:]
	}
	a.buffers[sessionID] = value
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
			a.status = "attached " + sessionID + "  Ctrl-] detach"
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
	if stream.cancel != nil {
		stream.cancel()
	}
	if stream.stream != nil {
		_ = stream.stream.Close()
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
}

func (a *App) streamSize() protocol.TerminalSize {
	cols, rows := 120, 36
	if a.In != nil {
		if c, r, err := term.Size(a.In); err == nil {
			cols, rows = c, r
		}
	}
	_, previewWidth, limit := appLayout(cols, rows)
	if previewWidth <= 0 {
		previewWidth = cols
	}
	if limit <= 0 {
		limit = rows
	}
	return protocol.TerminalSize{Cols: previewWidth, Rows: limit}
}

func (a *App) promptSlash(ctx context.Context) {
	value := a.promptInline("/")
	switch strings.TrimSpace(value) {
	case "login":
		a.promptLogin(ctx)
	case "refresh", "r":
		a.refresh(ctx)
	case "help", "?":
		a.status = "commands: /login /refresh /help"
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
	moveCursor(a.Out, 1, 1)
	hideCursor(a.Out)
	defer showCursor(a.Out)
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
	listWidth, previewWidth, limit := appLayout(cols, rows)
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
	writeLine(a.Out, cols, footer)
	if a.active != "" {
		writeLine(a.Out, cols, styleMuted("Ctrl-] detach  input is sent to session"))
	} else {
		writeLine(a.Out, cols, styleMuted("Enter/a attach  / commands  c create  s send  x stop  r refresh  ? help  q quit"))
	}
	clearToEnd(a.Out)
}

func (a *App) renderSplitRows(bodyStart, limit, listWidth, previewWidth, start int, previewLines []string) {
	for row := 0; row < limit; row++ {
		screenRow := bodyStart + row
		sessionIndex := start + row
		left := a.sessionListLine(row, sessionIndex, true)
		right := previewLine(previewLines, row)
		if len(a.preview) == 0 && row == 0 && len(a.sessions) > 0 {
			right = styleMuted("stream warming")
		}
		writeAt(a.Out, screenRow, listWidth+4, padVisible(truncateVisible(right, previewWidth), previewWidth))
		writeAt(a.Out, screenRow, listWidth+1, " | ")
		writeAt(a.Out, screenRow, 1, padVisible(left, listWidth))
	}
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
}

func (r *appKeyReader) Read(in io.Reader) (string, error) {
	for {
		key, ok, err := r.ReadAvailable(in)
		if err != nil || ok {
			return key, err
		}
	}
}

func (r *appKeyReader) ReadAvailable(in io.Reader) (string, bool, error) {
	if len(r.pending) > 0 {
		b := r.pending[0]
		r.pending = r.pending[1:]
		return keyFromByte(b), true, nil
	}
	var b [1]byte
	n, err := in.Read(b[:])
	if n == 0 {
		if err != nil && !errors.Is(err, io.EOF) {
			return "", false, err
		}
		return "", false, nil
	}
	switch b[0] {
	case 0x1b:
		seq := make([]byte, 0, 3)
		var next [1]byte
		for len(seq) < 3 {
			n, err := in.Read(next[:])
			if n > 0 {
				seq = append(seq, next[0])
				continue
			}
			if err != nil && !errors.Is(err, io.EOF) {
				return "", false, err
			}
			break
		}
		if len(seq) >= 2 && (seq[0] == '[' || seq[0] == 'O') {
			switch seq[1] {
			case 'A':
				return "up", true, nil
			case 'B':
				return "down", true, nil
			case 'C':
				return "right", true, nil
			case 'D':
				return "left", true, nil
			}
		}
		if len(seq) == 3 && seq[0] == '[' && seq[1] == '3' && seq[2] == '~' {
			return "delete", true, nil
		}
		if len(seq) > 0 {
			r.pending = append(r.pending, seq...)
		}
		return "unknown", true, nil
	default:
		return keyFromByte(b[0]), true, nil
	}
}

func (r *appKeyReader) Reset() {
	r.pending = nil
}

func keyFromByte(b byte) string {
	switch b {
	case '\r', '\n':
		return "enter"
	case 0x03:
		return "ctrl-c"
	case 0x1d:
		return "detach"
	case '\t':
		return "tab"
	case 0x7f, '\b':
		return string([]byte{b})
	default:
		return string(b)
	}
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

func appLayout(cols, rows int) (listWidth, previewWidth, limit int) {
	listWidth = cols
	previewWidth = 0
	if cols >= 100 {
		listWidth = min(max(28, cols/4), 34)
		previewWidth = cols - listWidth - 3
	}
	limit = rows - 7
	if limit < 4 {
		limit = 4
	}
	return listWidth, previewWidth, limit
}

func previewTitle(listWidth, previewWidth int, active string) string {
	if previewWidth <= 0 {
		return ""
	}
	title := styleHeader("Session")
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
	for i := 0; i < len(value); i++ {
		b := value[i]
		if b == 0x1b {
			i = skipEscape(value, i)
			continue
		}
		n++
	}
	return n
}

func truncateVisible(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	var out strings.Builder
	visible := 0
	for i := 0; i < len(value); i++ {
		b := value[i]
		if b == 0x1b {
			end := skipEscape(value, i)
			out.WriteString(value[i : end+1])
			i = end
			continue
		}
		out.WriteByte(b)
		visible++
		if visible >= limit {
			break
		}
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
