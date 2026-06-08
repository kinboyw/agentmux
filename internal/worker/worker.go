package worker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"private/agentmux/internal/protocol"
	"private/agentmux/internal/pty"
	"private/agentmux/internal/tmux"
	"private/agentmux/internal/ws"
)

type Worker struct {
	HubURL   string
	Token    string
	ID       string
	Name     string
	Version  string
	Adapter  tmux.Adapter
	Logger   *slog.Logger
	Interval time.Duration

	mu      sync.Mutex
	streams map[string]context.CancelFunc
	terms   map[string]*pty.Terminal
}

func New(hubURL, token, id, name string, adapter tmux.Adapter, logger *slog.Logger) *Worker {
	if logger == nil {
		logger = slog.Default()
	}
	if id == "" {
		id = name
	}
	if name == "" {
		name = id
	}
	return &Worker{
		HubURL: hubURL, Token: token, ID: id, Name: name, Version: "dev",
		Adapter: adapter, Logger: logger, Interval: time.Second,
		streams: map[string]context.CancelFunc{},
		terms:   map[string]*pty.Terminal{},
	}
}

func (w *Worker) Run(ctx context.Context) error {
	target, err := workerURL(w.HubURL, w.Token)
	if err != nil {
		return err
	}
	for {
		if err := w.runOnce(ctx, target); err != nil && ctx.Err() == nil {
			w.Logger.Error("worker connection failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (w *Worker) runOnce(ctx context.Context, target string) error {
	conn, err := ws.Dial(ctx, target, w.Token)
	if err != nil {
		return err
	}
	defer conn.Close()
	hello, _ := protocol.NewEnvelope(protocol.TypeWorkerHello, protocol.WorkerHello{Name: w.Name, Version: w.Version})
	hello.WorkerID = w.ID
	if err := writeEnvelope(conn, hello); err != nil {
		return err
	}
	w.Logger.Info("worker connected", "hub", target, "id", w.ID, "name", w.Name)
	if err := w.sendSnapshot(ctx, conn); err != nil {
		w.Logger.Error("snapshot failed", "error", err)
	}

	done := make(chan error, 1)
	go func() { done <- w.readLoop(ctx, conn) }()

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-done:
			w.stopAllStreams()
			return err
		case <-ticker.C:
			heartbeat := protocol.Envelope{Type: protocol.TypeWorkerHeartbeat, WorkerID: w.ID}
			if err := writeEnvelope(conn, heartbeat); err != nil {
				return err
			}
			_ = w.sendSnapshot(ctx, conn)
		}
	}
}

func (w *Worker) readLoop(ctx context.Context, conn *ws.Conn) error {
	for {
		env, err := readEnvelope(conn)
		if err != nil {
			return err
		}
		switch env.Type {
		case protocol.TypeSessionCreate:
			var session protocol.Session
			if err := env.DecodePayload(&session); err != nil {
				w.sendError(conn, env.SessionID, err.Error())
				continue
			}
			if err := w.Adapter.Create(ctx, session.Name, session.CWD, session.Command); err != nil {
				w.sendError(conn, protocol.SessionID(w.ID, session.Name), err.Error())
				continue
			}
			_ = w.sendSnapshot(ctx, conn)
		case protocol.TypeSessionKill:
			name := payloadName(env)
			if name == "" {
				_, name, _ = protocol.SplitSessionID(env.SessionID)
			}
			if err := w.Adapter.Kill(ctx, name); err != nil {
				w.sendError(conn, env.SessionID, err.Error())
				continue
			}
			w.stopSessionStreams(protocol.SessionID(w.ID, name))
			_ = w.sendSnapshot(ctx, conn)
		case protocol.TypeTerminalInput:
			_, name, ok := protocol.SplitSessionID(env.SessionID)
			if !ok {
				name = payloadName(env)
			}
			var input protocol.TerminalInput
			_ = env.DecodePayload(&input)
			if w.writeTerminal(env.StreamID, input.Data) {
				continue
			}
			if err := w.Adapter.SendTerminalInput(ctx, name, input.Data); err != nil {
				w.sendError(conn, env.SessionID, err.Error())
			}
		case protocol.TypeTerminalOpen:
			_, name, ok := protocol.SplitSessionID(env.SessionID)
			if !ok {
				name = payloadName(env)
			}
			var size protocol.TerminalSize
			_ = env.DecodePayload(&size)
			w.startStream(ctx, conn, env.StreamID, protocol.SessionID(w.ID, name), name, size)
		case protocol.TypeTerminalResize:
			var size protocol.TerminalSize
			if err := env.DecodePayload(&size); err != nil {
				w.sendStreamError(conn, env.StreamID, env.SessionID, err.Error())
				continue
			}
			if err := w.resizeTerminal(env.StreamID, size); err != nil {
				w.sendStreamError(conn, env.StreamID, env.SessionID, err.Error())
			}
		case protocol.TypeTerminalClose:
			w.stopStream(env.StreamID)
		}
	}
}

func (w *Worker) sendSnapshot(ctx context.Context, conn *ws.Conn) error {
	sessions, err := w.Adapter.List(ctx)
	if err != nil {
		return err
	}
	payload := protocol.SessionSnapshot{Sessions: make([]protocol.Session, 0, len(sessions))}
	for _, session := range sessions {
		payload.Sessions = append(payload.Sessions, protocol.Session{
			Name: session.Name, CWD: session.CWD, Command: session.Command, Status: session.Status,
		})
	}
	env, err := protocol.NewEnvelope(protocol.TypeSessionSnapshot, payload)
	if err != nil {
		return err
	}
	env.WorkerID = w.ID
	return writeEnvelope(conn, env)
}

func (w *Worker) startStream(parent context.Context, conn *ws.Conn, streamID, sessionID, name string, size protocol.TerminalSize) {
	if name == "" || streamID == "" {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	terminal, err := pty.StartTmuxAttach(ctx, name, size.Cols, size.Rows)
	if err != nil {
		w.sendStreamError(conn, streamID, sessionID, err.Error())
		cancel()
		return
	}
	w.mu.Lock()
	w.streams[streamID] = cancel
	w.terms[streamID] = terminal
	w.mu.Unlock()
	go func() {
		defer w.removeStream(streamID)
		buffer := make([]byte, 8192)
		for {
			n, err := terminal.Read(buffer)
			if n > 0 {
				env, _ := protocol.NewEnvelope(protocol.TypeTerminalOutput, protocol.TerminalOutput{
					Data:     base64.StdEncoding.EncodeToString(buffer[:n]),
					Encoding: "base64",
				})
				env.WorkerID = w.ID
				env.SessionID = sessionID
				env.StreamID = streamID
				_ = writeEnvelope(conn, env)
			}
			if err != nil {
				if err != io.EOF && ctx.Err() == nil && !strings.Contains(err.Error(), "input/output error") {
					w.sendStreamError(conn, streamID, sessionID, err.Error())
				}
				return
			}
		}
	}()
}

func (w *Worker) removeStream(streamID string) {
	w.mu.Lock()
	cancel := w.streams[streamID]
	delete(w.streams, streamID)
	terminal := w.terms[streamID]
	delete(w.terms, streamID)
	w.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if terminal != nil {
		_ = terminal.Close()
	}
}

func (w *Worker) writeTerminal(streamID, data string) bool {
	w.mu.Lock()
	terminal := w.terms[streamID]
	w.mu.Unlock()
	if terminal == nil {
		return false
	}
	_, err := terminal.Write([]byte(data))
	return err == nil
}

func (w *Worker) resizeTerminal(streamID string, size protocol.TerminalSize) error {
	w.mu.Lock()
	terminal := w.terms[streamID]
	w.mu.Unlock()
	if terminal == nil {
		return nil
	}
	return terminal.Resize(size.Cols, size.Rows)
}

func (w *Worker) stopStream(streamID string) {
	w.removeStream(streamID)
}

func (w *Worker) stopSessionStreams(sessionID string) {
	w.mu.Lock()
	streamIDs := make([]string, 0)
	for streamID := range w.terms {
		if strings.Contains(streamID, "|"+sessionID+"|") {
			streamIDs = append(streamIDs, streamID)
		}
	}
	w.mu.Unlock()
	for _, streamID := range streamIDs {
		w.stopStream(streamID)
	}
}

func (w *Worker) stopAllStreams() {
	w.mu.Lock()
	streams := w.streams
	w.streams = map[string]context.CancelFunc{}
	terms := w.terms
	w.terms = map[string]*pty.Terminal{}
	w.mu.Unlock()
	for _, cancel := range streams {
		cancel()
	}
	for _, terminal := range terms {
		_ = terminal.Close()
	}
}

func (w *Worker) sendError(conn *ws.Conn, sessionID, message string) {
	env, _ := protocol.NewEnvelope(protocol.TypeError, protocol.ErrorPayload{Message: message})
	env.WorkerID = w.ID
	env.SessionID = sessionID
	_ = writeEnvelope(conn, env)
}

func (w *Worker) sendStreamError(conn *ws.Conn, streamID, sessionID, message string) {
	env, _ := protocol.NewEnvelope(protocol.TypeError, protocol.ErrorPayload{Message: message})
	env.WorkerID = w.ID
	env.StreamID = streamID
	env.SessionID = sessionID
	_ = writeEnvelope(conn, env)
}

func ExchangeSignal(ctx context.Context, hubURL, signal, deviceID, deviceName string) (string, string, error) {
	base, err := httpBaseURL(hubURL)
	if err != nil {
		return "", "", err
	}
	req := map[string]string{
		"signal":      signal,
		"role":        "worker",
		"device_id":   deviceID,
		"device_name": deviceName,
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return "", "", err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/exchange", bytes.NewReader(raw))
	if err != nil {
		return "", "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", "", fmt.Errorf("POST /api/exchange failed: %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	var payload struct {
		Credential string `json:"credential"`
		DeviceID   string `json:"device_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", "", err
	}
	return payload.Credential, payload.DeviceID, nil
}

func workerURL(hubURL, token string) (string, error) {
	value := strings.TrimRight(hubURL, "/")
	if strings.HasPrefix(value, "http://") {
		value = "ws://" + strings.TrimPrefix(value, "http://")
	}
	if strings.HasPrefix(value, "https://") {
		value = "wss://" + strings.TrimPrefix(value, "https://")
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return "", err
	}
	if parsed.Scheme != "ws" && parsed.Scheme != "wss" {
		return "", fmt.Errorf("hub URL must use http(s) or ws(s)")
	}
	parsed.Path = "/ws/worker"
	if token != "" {
		q := parsed.Query()
		q.Set("token", token)
		parsed.RawQuery = q.Encode()
	}
	return parsed.String(), nil
}

func httpBaseURL(hubURL string) (string, error) {
	value := strings.TrimRight(hubURL, "/")
	if strings.HasPrefix(value, "ws://") {
		value = "http://" + strings.TrimPrefix(value, "ws://")
	}
	if strings.HasPrefix(value, "wss://") {
		value = "https://" + strings.TrimPrefix(value, "wss://")
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return "", err
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("hub URL must use http(s) or ws(s)")
	}
	parsed.Path = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return strings.TrimRight(parsed.String(), "/"), nil
}

func payloadName(env protocol.Envelope) string {
	var payload struct {
		Name string `json:"name"`
	}
	_ = env.DecodePayload(&payload)
	return payload.Name
}

func readEnvelope(conn *ws.Conn) (protocol.Envelope, error) {
	text, err := conn.ReadText()
	if err != nil {
		return protocol.Envelope{}, err
	}
	var env protocol.Envelope
	if err := json.Unmarshal([]byte(text), &env); err != nil {
		return protocol.Envelope{}, err
	}
	return env, env.Validate()
}

func writeEnvelope(conn *ws.Conn, env protocol.Envelope) error {
	raw, err := json.Marshal(env)
	if err != nil {
		return err
	}
	return conn.WriteText(string(raw))
}
