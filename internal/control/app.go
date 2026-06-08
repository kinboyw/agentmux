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

	rawRestore func()
	keys       appKeyReader
}

func NewApp(client Client, auth AppAuthResult, in *os.File, out io.Writer) *App {
	return &App{Client: client, Auth: auth, In: in, Out: out}
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
	a.status = "ready"
	a.refresh(ctx)
	a.render()
	for {
		key, err := a.keys.Read(a.In)
		if err != nil {
			return err
		}
		switch key {
		case "q":
			return nil
		case "up", "k":
			if a.selected > 0 {
				a.selected--
				a.refreshPreview(ctx)
			}
		case "down", "j":
			if a.selected < len(a.sessions)-1 {
				a.selected++
				a.refreshPreview(ctx)
			}
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
			a.refresh(ctx)
		case "?":
			a.status = "keys: up/down select, enter attach, c create, s send, x stop, r refresh, q quit"
		}
		a.clampSelection()
		a.render()
	}
}

func (a *App) refresh(ctx context.Context) {
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
	a.refreshPreview(ctx)
}

func (a *App) refreshPreview(ctx context.Context) {
	session := a.selectedSession()
	if session.ID == "" {
		a.preview = ""
		return
	}
	data, err := a.Client.SessionPreview(ctx, session.ID, 80)
	if err != nil {
		a.preview = ""
		a.err = err
		a.status = "preview failed"
		return
	}
	a.preview = data
	a.err = nil
}

func (a *App) promptCreate(ctx context.Context) {
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
	session := a.selectedSession()
	if session.ID == "" {
		a.status = "no session selected"
		return nil
	}
	a.disableRaw()
	term.ResetModes(a.Out)
	term.ExitAlternateScreen(a.Out)
	err := a.Client.Attach(ctx, session.ID, a.In, a.Out)
	term.ResetModes(a.Out)
	term.EnterAlternateScreen(a.Out)
	a.keys.Reset()
	if rawErr := a.enableRaw(); rawErr != nil && err == nil {
		err = rawErr
	}
	a.status = "detached " + session.ID
	if err == context.Canceled {
		return nil
	}
	return err
}

func (a *App) render() {
	cols, rows, err := term.Size(a.In)
	if err != nil {
		cols, rows = 120, 36
	}
	a.renderWithSize(cols, rows)
}

func (a *App) renderWithSize(cols, rows int) {
	clear(a.Out)
	source := a.Auth.Source
	if source == "" {
		source = "unknown"
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
			parts = append(parts, styleOK("*")+" "+worker.ID+styleMuted("("+worker.Name+")"))
		}
		workerSummary += strings.Join(parts, ", ")
	}
	writeLine(a.Out, cols, workerSummary)
	listWidth := cols
	previewWidth := 0
	if cols >= 100 {
		listWidth = min(max(38, cols*2/5), 48)
		previewWidth = cols - listWidth - 3
	}
	writeLine(a.Out, cols, styleHeader("Sessions")+previewTitle(listWidth, previewWidth))
	limit := rows - 7
	if limit < 4 {
		limit = 4
	}
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
	if a.err != nil {
		footer += styleMuted("  |  ") + styleError(a.err.Error())
	}
	writeLine(a.Out, cols, footer)
	writeLine(a.Out, cols, styleMuted("Enter/a attach  c create  s send  x stop  r refresh  ? help  q quit"))
}

func (a *App) renderSplitRows(bodyStart, limit, listWidth, previewWidth, start int, previewLines []string) {
	for row := 0; row < limit; row++ {
		screenRow := bodyStart + row
		sessionIndex := start + row
		left := a.sessionListLine(row, sessionIndex, true)
		right := previewLine(previewLines, row)
		if len(a.preview) == 0 && row == 0 && len(a.sessions) > 0 {
			right = styleMuted("no preview")
		}
		writeAt(a.Out, screenRow, listWidth+4, truncateVisible(right, previewWidth))
		writeAt(a.Out, screenRow, listWidth+1, " | ")
		writeAt(a.Out, screenRow, 1, padPlain(left, listWidth))
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
	if len(r.pending) > 0 {
		b := r.pending[0]
		r.pending = r.pending[1:]
		return keyFromByte(b), nil
	}
	var b [1]byte
	for {
		n, err := in.Read(b[:])
		if n > 0 {
			break
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
	}
	switch b[0] {
	case 0x1b:
		seq := make([]byte, 0, 2)
		var next [1]byte
		for len(seq) < 2 {
			n, err := in.Read(next[:])
			if n > 0 {
				seq = append(seq, next[0])
				continue
			}
			if err != nil && !errors.Is(err, io.EOF) {
				return "", err
			}
			break
		}
		if len(seq) == 2 && (seq[0] == '[' || seq[0] == 'O') {
			switch seq[1] {
			case 'A':
				return "up", nil
			case 'B':
				return "down", nil
			}
		}
		if len(seq) > 0 {
			r.pending = append(r.pending, seq...)
		}
		return "unknown", nil
	default:
		return keyFromByte(b[0]), nil
	}
}

func (r *appKeyReader) Reset() {
	r.pending = nil
}

func keyFromByte(b byte) string {
	switch b {
	case '\r', '\n':
		return "enter"
	default:
		return string(b)
	}
}

func clear(out io.Writer) {
	_, _ = io.WriteString(out, "\x1b[H\x1b[2J")
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
	if cols > 1 && visibleLen(line) > cols-1 {
		line = truncateVisible(line, cols-1)
	}
	_, _ = io.WriteString(out, line+"\r\n")
}

func previewTitle(listWidth, previewWidth int) string {
	if previewWidth <= 0 {
		return ""
	}
	return strings.Repeat(" ", max(0, listWidth-len("Sessions"))) + " | " + styleHeader("Preview")
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
		padPlain(ellipsizePlain(session.ID, 20), 21) +
		padPlain(ellipsizePlain(status, 9), 10) +
		ellipsizePlain(command, 9)
	return line
}

func padPlain(value string, width int) string {
	if width <= 0 {
		return value
	}
	value = stripControl(value)
	if len(value) >= width {
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
	return workers[0].ID
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
