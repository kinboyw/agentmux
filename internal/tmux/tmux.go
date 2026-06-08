package tmux

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var sessionNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,80}$`)

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

type Session struct {
	Name    string
	CWD     string
	Command string
	Status  string
}

func New(runner Runner) Adapter {
	if runner == nil {
		runner = ExecRunner{}
	}
	return Adapter{Runner: runner}
}

func (a Adapter) List(ctx context.Context) ([]Session, error) {
	output, err := a.Runner.Run(ctx, "tmux", "list-panes", "-a", "-F", "#{session_name}\t#{pane_current_path}\t#{pane_current_command}\t#{session_attached}")
	if err != nil {
		// tmux returns non-zero when no server exists. Treat that as empty.
		if strings.TrimSpace(output) == "" {
			return nil, nil
		}
		return nil, fmt.Errorf("tmux list-panes: %w: %s", err, strings.TrimSpace(output))
	}
	seen := map[string]Session{}
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
		seen[name] = Session{Name: name, CWD: parts[1], Command: parts[2], Status: status}
	}
	sessions := make([]Session, 0, len(seen))
	for _, session := range seen {
		sessions = append(sessions, session)
	}
	return sessions, nil
}

func (a Adapter) Create(ctx context.Context, name, cwd, command string) error {
	name = strings.TrimSpace(name)
	command = strings.TrimSpace(command)
	if !sessionNamePattern.MatchString(name) {
		return fmt.Errorf("invalid session name %q", name)
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
	if !sessionNamePattern.MatchString(name) {
		return fmt.Errorf("invalid session name %q", name)
	}
	if _, err := a.Runner.Run(ctx, "tmux", "kill-session", "-t", name); err != nil {
		return fmt.Errorf("tmux kill-session: %w", err)
	}
	return nil
}

func (a Adapter) SendInput(ctx context.Context, name, data string) error {
	if !sessionNamePattern.MatchString(name) {
		return fmt.Errorf("invalid session name %q", name)
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
	if !sessionNamePattern.MatchString(name) {
		return fmt.Errorf("invalid session name %q", name)
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
			switch data[i] {
			case '\r', '\n':
				tokens = append(tokens, "C-m")
			case 0x7f, '\b':
				tokens = append(tokens, "BSpace")
			case '\t':
				tokens = append(tokens, "Tab")
			case 0x03:
				tokens = append(tokens, "C-c")
			default:
				tokens = append(tokens, "literal:"+string(data[i]))
			}
			i++
		}
	}
	return tokens
}

func (a Adapter) Capture(ctx context.Context, name string, lines int) (string, error) {
	if !sessionNamePattern.MatchString(name) {
		return "", fmt.Errorf("invalid session name %q", name)
	}
	if lines <= 0 {
		lines = 200
	}
	output, err := a.Runner.Run(ctx, "tmux", "capture-pane", "-t", name, "-p", "-S", "-"+strconv.Itoa(lines))
	if err != nil {
		return "", fmt.Errorf("tmux capture-pane: %w", err)
	}
	return output, nil
}

func safeInt(value string) int {
	number, _ := strconv.Atoi(strings.TrimSpace(value))
	return number
}
