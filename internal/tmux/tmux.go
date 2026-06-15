package tmux

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"private/agentmux/internal/pty"
	"private/agentmux/internal/sessionbackend"

	"golang.org/x/sys/unix"
)

type Runner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	if name == "tmux" {
		path, err := ResolvePath()
		if err != nil {
			return "", err
		}
		name = path
	}
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

type Adapter struct {
	Runner Runner
}

const tmuxPaneGeometryFormat = "#{pane_id}\t#{pane_left}\t#{pane_top}\t#{pane_width}\t#{pane_height}\t#{window_width}\t#{window_height}"
const tmuxTargetFormat = "#{session_name}\t#{window_id}\t#{window_index}\t#{window_name}\t#{window_active}\t#{pane_id}\t#{pane_index}\t#{pane_active}\t#{pane_current_path}\t#{pane_current_command}\t#{pane_left}\t#{pane_top}\t#{pane_width}\t#{pane_height}"

type paneGeometry struct {
	id     string
	left   int
	top    int
	width  int
	height int
}

func CheckAvailable() error {
	_, err := ResolvePath()
	return err
}

func ResolvePath() (string, error) {
	if configured := strings.TrimSpace(firstNonEmptyEnv("AGENTMUX_TMUX", "AGENTMUX_TMUX_PATH")); configured != "" {
		if strings.ContainsAny(configured, `/\`) {
			if executable(configured) {
				return configured, nil
			}
			return "", fmt.Errorf("configured tmux path is not executable: %s", configured)
		}
		if path, err := exec.LookPath(configured); err == nil && path != "" {
			return path, nil
		}
		return "", fmt.Errorf("configured tmux command was not found: %s", configured)
	}
	if path, err := exec.LookPath("tmux"); err == nil && path != "" {
		return path, nil
	}
	candidates := []string{
		"/opt/homebrew/bin/tmux",
		"/usr/local/bin/tmux",
		"/opt/local/bin/tmux",
		"/usr/bin/tmux",
		"/bin/tmux",
	}
	for _, candidate := range candidates {
		if executable(candidate) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("tmux was not found. PATH=%q searched=%s\n\nInstall tmux for durable local sessions:\n  Debian/Ubuntu: sudo apt install tmux\n  Fedora:        sudo dnf install tmux\n  Arch:          sudo pacman -S tmux\n  macOS:         brew install tmux\n\nIf tmux is installed in a custom location, set AGENTMUX_TMUX=/path/to/tmux before starting the Worker. AgentMux can fall back to the built-in PTY backend, but built-in PTY sessions are lost when the worker process stops", os.Getenv("PATH"), strings.Join(candidates, ", "))
}

func firstNonEmptyEnv(keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return ""
}

func executable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}

func commandError(op string, err error, output string) error {
	message := strings.TrimSpace(output)
	if message == "" {
		return fmt.Errorf("%s: %w", op, err)
	}
	return fmt.Errorf("%s: %w: %s", op, err, message)
}

func New(runner Runner) Adapter {
	if runner == nil {
		runner = ExecRunner{}
	}
	return Adapter{Runner: runner}
}

func (a Adapter) Name() string {
	return "tmux"
}

func (a Adapter) List(ctx context.Context) ([]sessionbackend.Session, error) {
	output, err := a.Runner.Run(ctx, "tmux", "list-panes", "-a", "-F", "#{session_name}\t#{pane_current_path}\t#{pane_current_command}\t#{session_attached}")
	if err != nil {
		// tmux returns non-zero when no server exists. Treat that as empty.
		if strings.TrimSpace(output) == "" {
			return nil, nil
		}
		return nil, fmt.Errorf("tmux list-panes: %w: %s", err, strings.TrimSpace(output))
	}
	seen := map[string]sessionbackend.Session{}
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 4 {
			continue
		}
		name := parts[0]
		if _, ok := seen[name]; ok {
			continue
		}
		status := "idle"
		if safeInt(parts[3]) > 0 {
			status = "attached"
		}
		seen[name] = sessionbackend.Session{Name: name, CWD: parts[1], Command: parts[2], Status: status}
	}
	sessions := make([]sessionbackend.Session, 0, len(seen))
	for _, session := range seen {
		sessions = append(sessions, session)
	}
	slices.SortFunc(sessions, func(a, b sessionbackend.Session) int {
		return strings.Compare(a.Name, b.Name)
	})
	return sessions, nil
}

func (a Adapter) Create(ctx context.Context, name, cwd, command string) error {
	name = strings.TrimSpace(name)
	command = strings.TrimSpace(command)
	if err := sessionbackend.ValidateSessionName(name); err != nil {
		return err
	}
	if command == "" {
		return fmt.Errorf("command is required")
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("working directory %q is not accessible on worker: %w", abs, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("working directory %q is not a directory", abs)
	}
	output, err := a.Runner.Run(ctx, "tmux", "new-session", "-d", "-s", name, "-c", abs, command)
	if err != nil {
		return commandError("tmux new-session", err, output)
	}
	return nil
}

func (a Adapter) Kill(ctx context.Context, name string) error {
	if err := sessionbackend.ValidateSessionName(name); err != nil {
		return err
	}
	if _, err := a.Runner.Run(ctx, "tmux", "kill-session", "-t", name); err != nil {
		return fmt.Errorf("tmux kill-session: %w", err)
	}
	return nil
}

func (a Adapter) SendInput(ctx context.Context, name, data string) error {
	if err := sessionbackend.ValidateSessionName(name); err != nil {
		return err
	}
	if data == "" {
		return nil
	}
	lines := strings.SplitAfter(data, "\n")
	for _, part := range lines {
		if part == "" {
			continue
		}
		text := strings.TrimSuffix(part, "\n")
		if text != "" {
			if _, err := a.Runner.Run(ctx, "tmux", "send-keys", "-t", name, "-l", text); err != nil {
				return fmt.Errorf("tmux send-keys literal: %w", err)
			}
		}
		if strings.HasSuffix(part, "\n") {
			if _, err := a.Runner.Run(ctx, "tmux", "send-keys", "-t", name, "C-m"); err != nil {
				return fmt.Errorf("tmux send-keys carriage return: %w", err)
			}
		}
	}
	return nil
}

func (a Adapter) SendTerminalInput(ctx context.Context, name, data string) error {
	if err := sessionbackend.ValidateSessionName(name); err != nil {
		return err
	}
	return a.sendTerminalInput(ctx, name, data)
}

func (a Adapter) sendTerminalInput(ctx context.Context, target, data string) error {
	if data == "" {
		return nil
	}
	var literal strings.Builder
	flush := func() error {
		if literal.Len() == 0 {
			return nil
		}
		text := literal.String()
		literal.Reset()
		if _, err := a.Runner.Run(ctx, "tmux", "send-keys", "-t", target, "-l", text); err != nil {
			return fmt.Errorf("tmux send-keys literal: %w", err)
		}
		return nil
	}
	for _, token := range terminalTokens(data) {
		if strings.HasPrefix(token, "literal:") {
			literal.WriteString(strings.TrimPrefix(token, "literal:"))
			continue
		}
		if err := flush(); err != nil {
			return err
		}
		if _, err := a.Runner.Run(ctx, "tmux", "send-keys", "-t", target, token); err != nil {
			return fmt.Errorf("tmux send-keys %s: %w", token, err)
		}
	}
	return flush()
}

func terminalTokens(data string) []string {
	var tokens []string
	for i := 0; i < len(data); {
		switch {
		case strings.HasPrefix(data[i:], "\x1b[A"):
			tokens = append(tokens, "Up")
			i += 3
		case strings.HasPrefix(data[i:], "\x1b[B"):
			tokens = append(tokens, "Down")
			i += 3
		case strings.HasPrefix(data[i:], "\x1b[C"):
			tokens = append(tokens, "Right")
			i += 3
		case strings.HasPrefix(data[i:], "\x1b[D"):
			tokens = append(tokens, "Left")
			i += 3
		case strings.HasPrefix(data[i:], "\x1b[3~"):
			tokens = append(tokens, "Delete")
			i += 4
		default:
			if r, size := utf8.DecodeRuneInString(data[i:]); r != utf8.RuneError || size > 1 {
				if r >= utf8.RuneSelf {
					tokens = append(tokens, "literal:"+data[i:i+size])
					i += size
					continue
				}
			}
			switch data[i] {
			case 0x1b:
				tokens = append(tokens, "Escape")
			case '\r', '\n':
				tokens = append(tokens, "C-m")
			case 0x7f, '\b':
				tokens = append(tokens, "BSpace")
			case '\t':
				tokens = append(tokens, "Tab")
			case 0x03:
				tokens = append(tokens, "C-c")
			default:
				if token := controlKeyToken(data[i]); token != "" {
					tokens = append(tokens, token)
					i++
					continue
				}
				tokens = append(tokens, "literal:"+string(data[i]))
			}
			i++
		}
	}
	return tokens
}

func controlKeyToken(value byte) string {
	if value >= 0x01 && value <= 0x1a {
		return "C-" + string(rune('a'+value-1))
	}
	if value >= 0x1c && value <= 0x1f {
		return map[byte]string{
			0x1c: `C-\`,
			0x1d: "C-]",
			0x1e: "C-^",
			0x1f: "C-_",
		}[value]
	}
	return ""
}

func (a Adapter) Capture(ctx context.Context, name string, lines int) (string, error) {
	if err := sessionbackend.ValidateSessionName(name); err != nil {
		return "", err
	}
	if lines <= 0 {
		lines = 200
	}
	panes, windowWidth, windowHeight, err := a.windowPaneGeometry(ctx, name)
	if err == nil && len(panes) > 1 {
		return a.captureWindow(ctx, panes, windowWidth, windowHeight, lines)
	}
	return a.capturePaneScrollback(ctx, name, lines)
}

func (a Adapter) capturePaneScrollback(ctx context.Context, target string, lines int) (string, error) {
	output, err := a.Runner.Run(ctx, "tmux", "capture-pane", "-t", target, "-p", "-e", "-S", "-"+strconv.Itoa(lines))
	if err != nil {
		return "", fmt.Errorf("tmux capture-pane: %w", err)
	}
	return output, nil
}

func (a Adapter) windowPaneGeometry(ctx context.Context, name string) ([]paneGeometry, int, int, error) {
	output, err := a.Runner.Run(ctx, "tmux", "list-panes", "-t", name, "-F", tmuxPaneGeometryFormat)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("tmux list-panes: %w", err)
	}
	return parsePaneGeometry(output)
}

func parsePaneGeometry(output string) ([]paneGeometry, int, int, error) {
	panes := []paneGeometry(nil)
	windowWidth, windowHeight := 0, 0
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 7 {
			return nil, 0, 0, fmt.Errorf("tmux list-panes returned malformed pane geometry")
		}
		pane := paneGeometry{
			id:     strings.TrimSpace(parts[0]),
			left:   safeInt(parts[1]),
			top:    safeInt(parts[2]),
			width:  safeInt(parts[3]),
			height: safeInt(parts[4]),
		}
		if pane.id == "" || pane.width <= 0 || pane.height <= 0 {
			continue
		}
		windowWidth = max(windowWidth, safeInt(parts[5]), pane.left+pane.width)
		windowHeight = max(windowHeight, safeInt(parts[6]), pane.top+pane.height)
		panes = append(panes, pane)
	}
	if len(panes) == 0 {
		return nil, 0, 0, fmt.Errorf("tmux list-panes returned no panes")
	}
	return panes, windowWidth, windowHeight, nil
}

func (a Adapter) captureWindow(ctx context.Context, panes []paneGeometry, windowWidth, windowHeight, lines int) (string, error) {
	if windowWidth <= 0 || windowHeight <= 0 {
		return "", fmt.Errorf("tmux list-panes returned invalid window size")
	}
	canvasHeight := windowHeight
	if lines > 0 && lines < canvasHeight {
		canvasHeight = lines
	}
	canvas := newTextCanvas(windowWidth, canvasHeight)
	for _, pane := range panes {
		top, height := scalePaneVertical(pane.top, pane.height, windowHeight, canvasHeight)
		scaled := paneGeometry{id: pane.id, left: pane.left, top: top, width: pane.width, height: height}
		canvas.drawPaneBorder(scaled)
		output, err := a.Runner.Run(ctx, "tmux", "capture-pane", "-t", pane.id, "-p", "-e")
		if err != nil {
			return "", fmt.Errorf("tmux capture-pane %s: %w", pane.id, err)
		}
		canvas.drawPaneText(scaled, output)
	}
	return canvas.String(), nil
}

func scalePaneVertical(top, height, sourceHeight, targetHeight int) (int, int) {
	if sourceHeight <= 0 || targetHeight <= 0 {
		return 0, max(1, targetHeight)
	}
	scaledTop := top * targetHeight / sourceHeight
	scaledBottom := (top + height) * targetHeight / sourceHeight
	if scaledBottom <= scaledTop {
		scaledBottom = scaledTop + 1
	}
	if scaledTop < 0 {
		scaledTop = 0
	}
	if scaledBottom > targetHeight {
		scaledBottom = targetHeight
	}
	if scaledBottom <= scaledTop {
		scaledTop = max(0, targetHeight-1)
		scaledBottom = targetHeight
	}
	return scaledTop, scaledBottom - scaledTop
}

type textCanvas struct {
	width int
	rows  [][]rune
}

func newTextCanvas(width, height int) *textCanvas {
	rows := make([][]rune, max(0, height))
	for i := range rows {
		rows[i] = []rune(strings.Repeat(" ", max(0, width)))
	}
	return &textCanvas{width: width, rows: rows}
}

func (c *textCanvas) drawPaneBorder(pane paneGeometry) {
	if c == nil || c.width <= 0 || len(c.rows) == 0 || pane.width <= 0 || pane.height <= 0 {
		return
	}
	left := clamp(pane.left, 0, c.width-1)
	right := clamp(pane.left+pane.width-1, 0, c.width-1)
	top := clamp(pane.top, 0, len(c.rows)-1)
	bottom := clamp(pane.top+pane.height-1, 0, len(c.rows)-1)
	if right < left || bottom < top {
		return
	}
	for x := left; x <= right; x++ {
		c.rows[top][x] = '-'
		c.rows[bottom][x] = '-'
	}
	for y := top; y <= bottom; y++ {
		c.rows[y][left] = '|'
		c.rows[y][right] = '|'
	}
	c.rows[top][left] = '+'
	c.rows[top][right] = '+'
	c.rows[bottom][left] = '+'
	c.rows[bottom][right] = '+'
}

func (c *textCanvas) drawPaneText(pane paneGeometry, output string) {
	if c == nil || pane.width <= 2 || pane.height <= 2 {
		return
	}
	lines := plainCaptureLines(output)
	contentHeight := pane.height - 2
	if len(lines) > contentHeight {
		lines = lines[len(lines)-contentHeight:]
	}
	left := pane.left + 1
	top := pane.top + 1
	width := pane.width - 2
	for row, line := range lines {
		y := top + row
		if y < 0 || y >= len(c.rows) || left >= c.width {
			continue
		}
		x := max(0, left)
		for _, r := range []rune(line) {
			if x >= pane.left+1+width || x >= c.width {
				break
			}
			if x >= 0 {
				c.rows[y][x] = r
			}
			x++
		}
	}
}

func (c *textCanvas) String() string {
	if c == nil {
		return ""
	}
	lines := make([]string, 0, len(c.rows))
	for _, row := range c.rows {
		lines = append(lines, strings.TrimRight(string(row), " "))
	}
	return strings.Join(lines, "\n")
}

func plainCaptureLines(output string) []string {
	raw := strings.Split(strings.TrimRight(output, "\n"), "\n")
	if len(raw) == 1 && raw[0] == "" {
		return nil
	}
	lines := make([]string, 0, len(raw))
	for _, line := range raw {
		lines = append(lines, stripEscapeSequences(line))
	}
	return lines
}

func stripEscapeSequences(value string) string {
	var out strings.Builder
	for i := 0; i < len(value); i++ {
		b := value[i]
		if b == 0x1b {
			i = skipEscapeSequence(value, i)
			continue
		}
		if b < 0x20 && b != '\t' {
			continue
		}
		out.WriteByte(b)
	}
	return out.String()
}

func skipEscapeSequence(value string, start int) int {
	if start+1 >= len(value) {
		return start
	}
	switch value[start+1] {
	case '[':
		i := start + 2
		for i < len(value) {
			if value[i] >= 0x40 && value[i] <= 0x7e {
				return i
			}
			i++
		}
		return len(value) - 1
	case ']':
		i := start + 2
		for i < len(value) {
			if value[i] == 0x07 {
				return i
			}
			if value[i] == 0x1b && i+1 < len(value) && value[i+1] == '\\' {
				return i + 1
			}
			i++
		}
		return len(value) - 1
	default:
		return start + 1
	}
}

func clamp(value, low, high int) int {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

func (a Adapter) Open(ctx context.Context, name string, cols int, rows int) (sessionbackend.Stream, error) {
	if err := sessionbackend.ValidateSessionName(name); err != nil {
		return nil, err
	}
	tmuxPath, err := ResolvePath()
	if err != nil {
		return nil, err
	}
	return pty.StartCommand(ctx, tmuxPath, []string{"attach-session", "-t", name}, "", cols, rows)
}

func (a Adapter) Targets(ctx context.Context, name string) ([]sessionbackend.TerminalTarget, error) {
	if err := sessionbackend.ValidateSessionName(name); err != nil {
		return nil, err
	}
	output, err := a.Runner.Run(ctx, "tmux", "list-panes", "-s", "-t", name, "-F", tmuxTargetFormat)
	if err != nil {
		return nil, fmt.Errorf("tmux list-panes: %w", err)
	}
	return parseTargets(output)
}

func parseTargets(output string) ([]sessionbackend.TerminalTarget, error) {
	targets := []sessionbackend.TerminalTarget(nil)
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 14 {
			return nil, fmt.Errorf("tmux list-panes returned malformed target metadata")
		}
		target := sessionbackend.TerminalTarget{
			SessionName:  parts[0],
			WindowID:     parts[1],
			WindowIndex:  safeInt(parts[2]),
			WindowName:   parts[3],
			WindowActive: safeInt(parts[4]) > 0,
			PaneID:       parts[5],
			PaneIndex:    safeInt(parts[6]),
			PaneActive:   safeInt(parts[7]) > 0,
			CWD:          parts[8],
			Command:      parts[9],
			Left:         safeInt(parts[10]),
			Top:          safeInt(parts[11]),
			Width:        safeInt(parts[12]),
			Height:       safeInt(parts[13]),
		}
		if target.SessionName == "" || target.PaneID == "" {
			continue
		}
		targets = append(targets, target)
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("tmux list-panes returned no targets")
	}
	slices.SortFunc(targets, func(a, b sessionbackend.TerminalTarget) int {
		if a.WindowIndex != b.WindowIndex {
			return a.WindowIndex - b.WindowIndex
		}
		return a.PaneIndex - b.PaneIndex
	})
	return targets, nil
}

func (a Adapter) OpenTarget(ctx context.Context, target sessionbackend.TerminalTarget, cols int, rows int) (sessionbackend.Stream, error) {
	if target.PaneID == "" {
		return a.Open(ctx, target.SessionName, cols, rows)
	}
	if err := validateTmuxPaneID(target.PaneID); err != nil {
		return nil, err
	}
	stream, err := a.openPanePipe(ctx, target.PaneID)
	if err != nil {
		return nil, err
	}
	return stream, nil
}

func (a Adapter) CaptureTarget(ctx context.Context, target sessionbackend.TerminalTarget, lines int) (string, error) {
	if target.PaneID == "" {
		return a.Capture(ctx, target.SessionName, lines)
	}
	if err := validateTmuxPaneID(target.PaneID); err != nil {
		return "", err
	}
	if lines <= 0 {
		lines = 80
	}
	return a.capturePaneScrollback(ctx, target.PaneID, lines)
}

func (a Adapter) CaptureTargetScreen(ctx context.Context, target sessionbackend.TerminalTarget) (string, error) {
	if target.PaneID == "" {
		return a.Capture(ctx, target.SessionName, target.Height)
	}
	if err := validateTmuxPaneID(target.PaneID); err != nil {
		return "", err
	}
	output, err := a.Runner.Run(ctx, "tmux", "capture-pane", "-t", target.PaneID, "-p", "-e", "-S", "0", "-E", "-")
	if err != nil {
		return "", fmt.Errorf("tmux capture-pane screen: %w", err)
	}
	return output, nil
}

func validateTmuxPaneID(value string) error {
	if !strings.HasPrefix(value, "%") || len(value) < 2 {
		return fmt.Errorf("invalid tmux pane id %q", value)
	}
	for _, r := range value[1:] {
		if r < '0' || r > '9' {
			return fmt.Errorf("invalid tmux pane id %q", value)
		}
	}
	return nil
}

func (a Adapter) openPanePipe(ctx context.Context, paneID string) (sessionbackend.Stream, error) {
	dir, err := os.MkdirTemp("", "agentmux-tmux-pane-*")
	if err != nil {
		return nil, err
	}
	fifo := filepath.Join(dir, "output")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("tmux pane fifo: %w", err)
	}
	command := "cat > " + shellQuote(fifo)
	if _, err := a.Runner.Run(ctx, "tmux", "pipe-pane", "-t", paneID, command); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("tmux pipe-pane: %w", err)
	}
	file, err := os.OpenFile(fifo, os.O_RDWR, 0)
	if err != nil {
		_, _ = a.Runner.Run(context.Background(), "tmux", "pipe-pane", "-t", paneID)
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("tmux pane fifo open: %w", err)
	}
	return &panePipeStream{
		ctx:    ctx,
		runner: a.Runner,
		paneID: paneID,
		dir:    dir,
		file:   file,
	}, nil
}

type panePipeStream struct {
	ctx       context.Context
	runner    Runner
	paneID    string
	dir       string
	file      *os.File
	closeOnce sync.Once
}

func (s *panePipeStream) Read(p []byte) (int, error) {
	if s.file == nil {
		return 0, os.ErrClosed
	}
	return s.file.Read(p)
}

func (s *panePipeStream) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if err := New(s.runner).sendTerminalInput(s.ctx, s.paneID, string(p)); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (s *panePipeStream) Resize(cols int, rows int) error {
	return nil
}

func (s *panePipeStream) Close() error {
	var err error
	s.closeOnce.Do(func() {
		if s.runner != nil && s.paneID != "" {
			_, _ = s.runner.Run(context.Background(), "tmux", "pipe-pane", "-t", s.paneID)
		}
		if s.file != nil {
			err = s.file.Close()
		}
		if s.dir != "" {
			_ = os.RemoveAll(s.dir)
		}
	})
	return err
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func safeInt(value string) int {
	number, _ := strconv.Atoi(strings.TrimSpace(value))
	return number
}
