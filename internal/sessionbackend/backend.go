package sessionbackend

import (
	"context"
	"fmt"
	"regexp"
)

var sessionNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,80}$`)

type Session struct {
	Name    string
	CWD     string
	Command string
	Status  string
}

type Stream interface {
	Read([]byte) (int, error)
	Write([]byte) (int, error)
	Resize(cols int, rows int) error
	Close() error
}

type Backend interface {
	Name() string
	List(ctx context.Context) ([]Session, error)
	Create(ctx context.Context, name, cwd, command string) error
	Kill(ctx context.Context, name string) error
	SendTerminalInput(ctx context.Context, name, data string) error
	Capture(ctx context.Context, name string, lines int) (string, error)
	Open(ctx context.Context, name string, cols int, rows int) (Stream, error)
}

type TerminalTarget struct {
	SessionName  string
	WindowID     string
	WindowIndex  int
	WindowName   string
	WindowActive bool
	PaneID       string
	PaneIndex    int
	PaneActive   bool
	CWD          string
	Command      string
	Left         int
	Top          int
	Width        int
	Height       int
}

type TargetBackend interface {
	Targets(ctx context.Context, name string) ([]TerminalTarget, error)
	OpenTarget(ctx context.Context, target TerminalTarget, cols int, rows int) (Stream, error)
}

func ValidateSessionName(name string) error {
	if !sessionNamePattern.MatchString(name) {
		return fmt.Errorf("invalid session name %q", name)
	}
	return nil
}
