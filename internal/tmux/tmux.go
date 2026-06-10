package tmux

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"private/agentmux/internal/pty"
	"private/agentmux/internal/sessionbackend"
)

type Runner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

type Adapter struct {
	Runner Runner
}

const tmuxPaneGeometryFormat = "#{pane_id}\t#{pane_left}\t#{pane_top}\t#{pane_width}\t#{pane_height}\t#{window_width}\t#{window_height}"

type paneGeometry struct {
	id     string
	left   int
	top    int
	width  int
	height int
}

func CheckAvailable() error {
	path, err := exec.LookPath("tmux")
	if err != nil {
		return fmt.Errorf("tmux was not found in PATH\n\nInstall tmux for durable local sessions:\n  Debian/Ubuntu: sudo apt install tmux\n  Fedora:        sudo dnf install tmux\n  Arch:          sudo pacman -S tmux\n  macOS:         brew install tmux\n\nAgentMux can fall back to the built-in PTY backend, but built-in PTY sessions are lost when the worker process stops")
	}
	if path == "" {
		return fmt.Errorf("tmux was not found in PATH")
	}
	return nil
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
	if _, err := a.Runner.Run(ctx, "tmux", "new-session", "-d", "-s", name, "-c", abs, command); err != nil {
		return fmt.Errorf("tmux new-session: %w", err)
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
		if _, err := a.Runner.Run(ctx, "tmux", "send-keys", "-t", name, "-l", text); err != nil {
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
		if _, err := a.Runner.Run(ctx, "tmux", "send-keys", "-t", name, token); err != nil {
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
	return pty.StartTmuxAttach(ctx, name, cols, rows)
}

func safeInt(value string) int {
	number, _ := strconv.Atoi(strings.TrimSpace(value))
	return number
}
