package ptybackend

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"private/agentmux/internal/pty"
	"private/agentmux/internal/sessionbackend"
)

const maxHistoryBytes = 256 * 1024

type Backend struct {
	mu       sync.Mutex
	nextID   uint64
	sessions map[string]*session
}

type session struct {
	name    string
	cwd     string
	command string
	term    *pty.Terminal
	done    chan struct{}

	mu      sync.Mutex
	status  string
	history string
	streams map[uint64]*stream
}

type stream struct {
	id      uint64
	session *session
	ch      chan []byte

	mu      sync.Mutex
	pending []byte
	closed  bool
}

func New() *Backend {
	return &Backend{sessions: map[string]*session{}}
}

func (b *Backend) Name() string {
	return "pty"
}

func (b *Backend) List(ctx context.Context) ([]sessionbackend.Session, error) {
	_ = ctx
	b.mu.Lock()
	sessions := make([]*session, 0, len(b.sessions))
	for _, s := range b.sessions {
		sessions = append(sessions, s)
	}
	b.mu.Unlock()

	result := make([]sessionbackend.Session, 0, len(sessions))
	for _, s := range sessions {
		s.mu.Lock()
		result = append(result, sessionbackend.Session{
			Name: s.name, CWD: s.cwd, Command: s.command, Status: s.status,
		})
		s.mu.Unlock()
	}
	slices.SortFunc(result, func(a, b sessionbackend.Session) int {
		return strings.Compare(a.Name, b.Name)
	})
	return result, nil
}

func (b *Backend) Create(ctx context.Context, name, cwd, command string) error {
	name = strings.TrimSpace(name)
	command = strings.TrimSpace(command)
	if err := sessionbackend.ValidateSessionName(name); err != nil {
		return err
	}
	if command == "" {
		return fmt.Errorf("command is required")
	}
	if cwd == "" {
		cwd = "."
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return err
	}

	b.mu.Lock()
	if _, exists := b.sessions[name]; exists {
		b.mu.Unlock()
		return fmt.Errorf("session %q already exists", name)
	}
	b.mu.Unlock()

	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	term, err := pty.StartCommand(ctx, shell, []string{"-lc", command}, abs, 120, 36)
	if err != nil {
		return fmt.Errorf("start pty session: %w", err)
	}
	s := &session{
		name: name, cwd: abs, command: command, term: term, done: make(chan struct{}),
		status: "idle", streams: map[uint64]*stream{},
	}

	b.mu.Lock()
	if _, exists := b.sessions[name]; exists {
		b.mu.Unlock()
		_ = term.Kill()
		_ = term.Close()
		return fmt.Errorf("session %q already exists", name)
	}
	b.sessions[name] = s
	b.mu.Unlock()

	go b.readSession(ctx, s)
	go b.waitSession(name, s)
	return nil
}

func (b *Backend) Close() error {
	b.mu.Lock()
	sessions := make([]*session, 0, len(b.sessions))
	for _, s := range b.sessions {
		sessions = append(sessions, s)
	}
	b.mu.Unlock()
	for _, s := range sessions {
		_ = s.term.Kill()
		_ = s.term.Close()
		b.remove(s.name, s)
	}
	return nil
}

func (b *Backend) Kill(ctx context.Context, name string) error {
	_ = ctx
	if err := sessionbackend.ValidateSessionName(name); err != nil {
		return err
	}
	s, ok := b.get(name)
	if !ok {
		return fmt.Errorf("session %q not found", name)
	}
	_ = s.term.Kill()
	_ = s.term.Close()
	b.remove(name, s)
	return nil
}

func (b *Backend) SendTerminalInput(ctx context.Context, name, data string) error {
	_ = ctx
	if data == "" {
		return nil
	}
	s, ok := b.get(name)
	if !ok {
		return fmt.Errorf("session %q not found", name)
	}
	_, err := s.term.Write([]byte(data))
	return err
}

func (b *Backend) Capture(ctx context.Context, name string, lines int) (string, error) {
	_ = ctx
	s, ok := b.get(name)
	if !ok {
		return "", fmt.Errorf("session %q not found", name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return captureLines(s.history, lines), nil
}

func (b *Backend) Open(ctx context.Context, name string, cols int, rows int) (sessionbackend.Stream, error) {
	_ = ctx
	s, ok := b.get(name)
	if !ok {
		return nil, fmt.Errorf("session %q not found", name)
	}
	if err := s.term.Resize(cols, rows); err != nil {
		return nil, err
	}
	stream := &stream{
		id: atomic.AddUint64(&b.nextID, 1), session: s, ch: make(chan []byte, 64),
	}
	s.mu.Lock()
	s.streams[stream.id] = stream
	initial := captureLines(s.history, 200)
	s.mu.Unlock()
	if initial != "" {
		stream.offer([]byte(initial))
	}
	return stream, nil
}

func (b *Backend) get(name string) (*session, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.sessions[name]
	return s, ok
}

func (b *Backend) readSession(ctx context.Context, s *session) {
	_ = s.term.CopyOutput(ctx, func(chunk []byte) {
		s.mu.Lock()
		s.history += string(chunk)
		if len(s.history) > maxHistoryBytes {
			s.history = s.history[len(s.history)-maxHistoryBytes:]
		}
		streams := make([]*stream, 0, len(s.streams))
		for _, stream := range s.streams {
			streams = append(streams, stream)
		}
		s.mu.Unlock()
		for _, stream := range streams {
			stream.offer(chunk)
		}
	})
}

func (b *Backend) waitSession(name string, s *session) {
	_ = s.term.Wait()
	b.remove(name, s)
}

func (b *Backend) remove(name string, s *session) {
	b.mu.Lock()
	if b.sessions[name] == s {
		delete(b.sessions, name)
	}
	b.mu.Unlock()

	s.mu.Lock()
	if s.status != "exited" {
		s.status = "exited"
	}
	streams := make([]*stream, 0, len(s.streams))
	for _, stream := range s.streams {
		streams = append(streams, stream)
	}
	s.streams = map[uint64]*stream{}
	s.mu.Unlock()

	for _, stream := range streams {
		stream.closeChannel()
	}
	select {
	case <-s.done:
	default:
		close(s.done)
	}
}

func (s *session) removeStream(id uint64) {
	s.mu.Lock()
	stream := s.streams[id]
	delete(s.streams, id)
	s.mu.Unlock()
	if stream != nil {
		stream.closeChannel()
	}
}

func (s *stream) Read(p []byte) (int, error) {
	s.mu.Lock()
	if len(s.pending) > 0 {
		n := copy(p, s.pending)
		s.pending = s.pending[n:]
		s.mu.Unlock()
		return n, nil
	}
	if s.closed {
		s.mu.Unlock()
		return 0, io.EOF
	}
	s.mu.Unlock()

	chunk, ok := <-s.ch
	if !ok {
		return 0, io.EOF
	}
	n := copy(p, chunk)
	if n < len(chunk) {
		s.mu.Lock()
		s.pending = append(s.pending, chunk[n:]...)
		s.mu.Unlock()
	}
	return n, nil
}

func (s *stream) Write(p []byte) (int, error) {
	return s.session.term.Write(p)
}

func (s *stream) Resize(cols int, rows int) error {
	return s.session.term.Resize(cols, rows)
}

func (s *stream) Close() error {
	s.session.removeStream(s.id)
	return nil
}

func (s *stream) offer(chunk []byte) {
	data := append([]byte(nil), chunk...)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	select {
	case s.ch <- data:
	default:
	}
}

func (s *stream) closeChannel() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	close(s.ch)
}

func captureLines(value string, lines int) string {
	if lines <= 0 {
		lines = 200
	}
	parts := strings.SplitAfter(value, "\n")
	if len(parts) <= lines {
		return value
	}
	return strings.Join(parts[len(parts)-lines:], "")
}
