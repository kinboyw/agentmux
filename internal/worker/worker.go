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

	"private/agentmux/internal/credentialcache"
	"private/agentmux/internal/protocol"
	"private/agentmux/internal/sessionbackend"
	"private/agentmux/internal/ws"
)

const (
	workerPingInterval = 15 * time.Second
	workerPongWait     = 45 * time.Second
)

type AuthOptions struct {
	HubURL     string
	Token      string
	Join       string
	DeviceID   string
	DeviceName string
}

type AuthResult struct {
	HubURL       string
	Token        string
	CredentialID string
	TenantID     string
	DeviceID     string
	DeviceName   string
	Role         string
	ExpiresAt    time.Time
	Source       string
}

type ExchangedCredential struct {
	Credential   string    `json:"credential"`
	CredentialID string    `json:"credential_id"`
	TenantID     string    `json:"tenant_id"`
	Role         string    `json:"role"`
	DeviceID     string    `json:"device_id"`
	ExpiresAt    time.Time `json:"expires_at"`
	Scopes       []string  `json:"scopes"`
}

type Worker struct {
	HubURL   string
	Token    string
	ID       string
	Name     string
	Version  string
	Backend  sessionbackend.Backend
	Logger   *slog.Logger
	Interval time.Duration

	mu      sync.Mutex
	streams map[string]context.CancelFunc
	terms   map[string]sessionbackend.Stream
}

func New(hubURL, token, id, name string, backend sessionbackend.Backend, logger *slog.Logger) *Worker {
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
		Backend: backend, Logger: logger, Interval: time.Second,
		streams: map[string]context.CancelFunc{},
		terms:   map[string]sessionbackend.Stream{},
	}
}

func ResolveAuth(ctx context.Context, opts AuthOptions) (AuthResult, error) {
	hubURL := credentialcache.NormalizeHubURL(opts.HubURL)
	deviceID := strings.TrimSpace(opts.DeviceID)
	deviceName := strings.TrimSpace(opts.DeviceName)
	if deviceName == "" {
		deviceName = "worker"
	}
	if deviceID == "" && (opts.Join != "" || opts.Token != "") {
		deviceID = deviceName
	}
	if opts.Join != "" {
		credential, err := ExchangeSignalDetail(ctx, hubURL, opts.Join, deviceID, deviceName)
		if err != nil {
			return AuthResult{}, err
		}
		entry := credentialcache.Entry{
			HubURL: hubURL, Credential: credential.Credential, CredentialID: credential.CredentialID,
			TenantID: credential.TenantID, Role: credential.Role, DeviceID: credential.DeviceID,
			DeviceName: deviceName, ExpiresAt: credential.ExpiresAt, UpdatedAt: time.Now().UTC(),
		}
		_ = credentialcache.Save(entry)
		return AuthResult{
			HubURL: hubURL, Token: credential.Credential, CredentialID: credential.CredentialID,
			TenantID: credential.TenantID, DeviceID: credential.DeviceID, DeviceName: deviceName,
			Role: credential.Role, ExpiresAt: credential.ExpiresAt, Source: "join",
		}, nil
	}
	if opts.Token != "" {
		return AuthResult{HubURL: hubURL, Token: opts.Token, DeviceID: deviceID, DeviceName: deviceName, Source: "token"}, nil
	}
	var entry credentialcache.Entry
	var ok bool
	if hubURL == "" {
		entry, ok = credentialcache.LoadLatest("worker", deviceID)
	} else {
		entry, ok = credentialcache.Load(hubURL, "worker", deviceID)
	}
	if !ok {
		return AuthResult{}, fmt.Errorf("no credential available; pass --join or --token")
	}
	return AuthResult{
		HubURL: entry.HubURL, Token: entry.Credential, CredentialID: entry.CredentialID,
		TenantID: entry.TenantID, DeviceID: entry.DeviceID, DeviceName: entry.DeviceName,
		Role: entry.Role, ExpiresAt: entry.ExpiresAt, Source: "cache",
	}, nil
}

func (w *Worker) Run(ctx context.Context) error {
	defer w.closeBackend()
	target, err := workerURL(w.HubURL, w.Token)
	if err != nil {
		return err
	}
	backoff := 2 * time.Second
	for {
		if err := w.runOnce(ctx, target); err != nil && ctx.Err() == nil {
			w.Logger.Error("worker connection failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
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
	w.Logger.Info("worker connected", "hub", target, "id", w.ID, "name", w.Name, "session_backend", w.Backend.Name())
	if err := w.sendSnapshot(ctx, conn); err != nil {
		w.Logger.Error("snapshot failed", "error", err)
	}
	_ = conn.SetReadTimeout(workerPongWait)

	done := make(chan error, 1)
	go func() { done <- w.readLoop(ctx, conn) }()

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	pingTicker := time.NewTicker(workerPingInterval)
	defer pingTicker.Stop()
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
		case <-pingTicker.C:
			if err := conn.WritePing([]byte("agentmux")); err != nil {
				return err
			}
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
		case protocol.TypeSessionSync:
			_ = w.sendSnapshot(ctx, conn)
		case protocol.TypeSessionCreate:
			var session protocol.Session
			if err := env.DecodePayload(&session); err != nil {
				w.sendError(conn, env.SessionID, err.Error())
				continue
			}
			if err := w.Backend.Create(ctx, session.Name, session.CWD, session.Command); err != nil {
				w.sendError(conn, protocol.SessionID(w.ID, session.Name), err.Error())
				continue
			}
			_ = w.sendSnapshot(ctx, conn)
		case protocol.TypeSessionKill:
			name := payloadName(env)
			if name == "" {
				_, name, _ = protocol.SplitSessionID(env.SessionID)
			}
			if err := w.Backend.Kill(ctx, name); err != nil {
				w.sendError(conn, env.SessionID, err.Error())
				continue
			}
			w.stopSessionStreams(protocol.SessionID(w.ID, name))
			_ = w.sendSnapshot(ctx, conn)
		case protocol.TypeSessionPreview:
			_, name, ok := protocol.SplitSessionID(env.SessionID)
			if !ok {
				name = payloadName(env)
			}
			var req protocol.SessionPreviewRequest
			_ = env.DecodePayload(&req)
			data, err := w.Backend.Capture(ctx, name, req.Lines)
			if err != nil {
				w.sendRequestError(conn, env.ID, env.SessionID, err.Error())
				continue
			}
			reply, _ := protocol.NewEnvelope(protocol.TypeSessionPreview, protocol.SessionPreview{Data: data})
			reply.ID = env.ID
			reply.WorkerID = w.ID
			reply.SessionID = env.SessionID
			_ = writeEnvelope(conn, reply)
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
			if err := w.Backend.SendTerminalInput(ctx, name, input.Data); err != nil {
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
	sessions, err := w.Backend.List(ctx)
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
	terminal, err := w.Backend.Open(ctx, name, size.Cols, size.Rows)
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
	w.terms = map[string]sessionbackend.Stream{}
	w.mu.Unlock()
	for _, cancel := range streams {
		cancel()
	}
	for _, terminal := range terms {
		_ = terminal.Close()
	}
}

func (w *Worker) closeBackend() {
	closer, ok := w.Backend.(interface{ Close() error })
	if ok {
		_ = closer.Close()
	}
}

func (w *Worker) sendError(conn *ws.Conn, sessionID, message string) {
	env, _ := protocol.NewEnvelope(protocol.TypeError, protocol.ErrorPayload{Message: message})
	env.WorkerID = w.ID
	env.SessionID = sessionID
	_ = writeEnvelope(conn, env)
}

func (w *Worker) sendRequestError(conn *ws.Conn, requestID, sessionID, message string) {
	env, _ := protocol.NewEnvelope(protocol.TypeError, protocol.ErrorPayload{Message: message})
	env.ID = requestID
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
	payload, err := ExchangeSignalDetail(ctx, hubURL, signal, deviceID, deviceName)
	if err != nil {
		return "", "", err
	}
	return payload.Credential, payload.DeviceID, nil
}

func ExchangeSignalDetail(ctx context.Context, hubURL, signal, deviceID, deviceName string) (ExchangedCredential, error) {
	base, err := httpBaseURL(hubURL)
	if err != nil {
		return ExchangedCredential{}, err
	}
	req := map[string]string{
		"signal":      signal,
		"role":        "worker",
		"device_id":   deviceID,
		"device_name": deviceName,
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return ExchangedCredential{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/exchange", bytes.NewReader(raw))
	if err != nil {
		return ExchangedCredential{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return ExchangedCredential{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return ExchangedCredential{}, fmt.Errorf("POST /api/exchange failed: %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	var payload ExchangedCredential
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return ExchangedCredential{}, err
	}
	return payload, nil
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
