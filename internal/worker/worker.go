package worker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
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
	InstanceID string
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

type ExchangeHTTPError struct {
	StatusCode int
	Status     string
	Body       string
}

func (e ExchangeHTTPError) Error() string {
	return fmt.Sprintf("POST /api/exchange failed: %s: %s", e.Status, e.Body)
}

type Worker struct {
	HubURL   string
	Token    string
	ID       string
	Name     string
	Version  string
	Software protocol.WorkerSoftware
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
		Software: defaultWorkerSoftware(),
		Backend:  backend, Logger: logger, Interval: time.Second,
		streams: map[string]context.CancelFunc{},
		terms:   map[string]sessionbackend.Stream{},
	}
}

func defaultWorkerSoftware() protocol.WorkerSoftware {
	return protocol.WorkerSoftware{
		Version:         "dev",
		GoVersion:       runtime.Version(),
		OS:              runtime.GOOS,
		Arch:            runtime.GOARCH,
		ProtocolVersion: protocol.ProtocolVersion,
		Capabilities:    append([]string(nil), protocol.DefaultWorkerCapabilities...),
		InstallKind:     getenv("AGENTMUX_WORKER_INSTALL_KIND", "process"),
		ServiceBackend:  getenv("AGENTMUX_WORKER_SERVICE_BACKEND", "process"),
		UpdateChannel:   getenv("AGENTMUX_UPDATE_CHANNEL", "stable"),
		UpdatePolicy:    getenv("AGENTMUX_UPDATE_POLICY", "manual"),
	}
}

func getenv(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func IsRetryableAuthError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var httpErr ExchangeHTTPError
	if errors.As(err, &httpErr) {
		return httpErr.StatusCode == http.StatusRequestTimeout || httpErr.StatusCode == http.StatusTooManyRequests || httpErr.StatusCode >= 500
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return !errors.Is(urlErr.Err, context.Canceled)
	}
	return false
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
		if existing, ok := credentialcache.LoadLatest("worker", ""); ok {
			existingHub := credentialcache.NormalizeHubURL(existing.HubURL)
			if existingHub != "" && hubURL != "" && existingHub != hubURL {
				return AuthResult{}, fmt.Errorf("worker is already joined to %s; run worker leave before joining %s", existingHub, hubURL)
			}
		}
		credential, err := ExchangeSignalDetail(ctx, hubURL, opts.Join, deviceID, deviceName, opts.InstanceID)
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
			w.Logger.Error("worker connection failed; retrying", "retry_in", backoff.String(), "error", err)
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
	software := w.Software
	if software.Version == "" {
		software.Version = w.Version
	}
	hello, _ := protocol.NewEnvelope(protocol.TypeWorkerHello, protocol.WorkerHello{Name: w.Name, Backend: w.Backend.Name(), WorkerSoftware: software})
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
			w.Logger.Debug("worker websocket read ended", "error", err)
			return err
		}
		w.Logger.Debug("worker message received", "type", env.Type, "session_id", env.SessionID, "stream_id", env.StreamID, "request_id", env.ID, "payload_bytes", len(env.Payload))
		switch env.Type {
		case protocol.TypeSessionSync:
			_ = w.sendSnapshot(ctx, conn)
		case protocol.TypeSessionCreate:
			var session protocol.Session
			if err := env.DecodePayload(&session); err != nil {
				w.sendRequestError(conn, env.ID, env.SessionID, err.Error())
				continue
			}
			sessionID := protocol.SessionID(w.ID, session.Name)
			if err := w.Backend.Create(ctx, session.Name, session.CWD, session.Command); err != nil {
				w.sendRequestError(conn, env.ID, sessionID, err.Error())
				continue
			}
			reply, _ := protocol.NewEnvelope(protocol.TypeSessionCreated, protocol.Session{
				Name: session.Name, CWD: session.CWD, Command: session.Command, Status: "running", Backend: w.Backend.Name(),
			})
			reply.ID = env.ID
			reply.WorkerID = w.ID
			reply.SessionID = sessionID
			_ = writeEnvelope(conn, reply)
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
			w.Logger.Debug("preview capture start", "session_id", env.SessionID, "name", name, "request_id", env.ID, "lines", req.Lines)
			data, err := w.Backend.Capture(ctx, name, req.Lines)
			if err != nil {
				w.Logger.Debug("preview capture failed", "session_id", env.SessionID, "name", name, "request_id", env.ID, "error", err)
				w.sendRequestError(conn, env.ID, env.SessionID, err.Error())
				continue
			}
			w.Logger.Debug("preview capture complete", "session_id", env.SessionID, "name", name, "request_id", env.ID, "bytes", len(data))
			reply, _ := protocol.NewEnvelope(protocol.TypeSessionPreview, protocol.SessionPreview{Data: data, Scope: "active_pane"})
			reply.ID = env.ID
			reply.WorkerID = w.ID
			reply.SessionID = env.SessionID
			_ = writeEnvelope(conn, reply)
		case protocol.TypeSessionTargets:
			_, name, ok := protocol.SplitSessionID(env.SessionID)
			if !ok {
				name = payloadName(env)
			}
			w.Logger.Debug("targets request received", "session_id", env.SessionID, "name", name, "request_id", env.ID)
			targets := []protocol.TerminalTarget{{SessionName: name}}
			if backend, ok := w.Backend.(sessionbackend.TargetBackend); ok {
				items, err := backend.Targets(ctx, name)
				if err != nil {
					w.Logger.Debug("targets request failed", "session_id", env.SessionID, "name", name, "request_id", env.ID, "error", err)
					w.sendRequestError(conn, env.ID, env.SessionID, err.Error())
					continue
				}
				targets = protocolTargets(items)
			}
			reply, _ := protocol.NewEnvelope(protocol.TypeSessionTargets, protocol.SessionTargets{Targets: targets})
			reply.ID = env.ID
			reply.WorkerID = w.ID
			reply.SessionID = env.SessionID
			w.Logger.Debug("targets request complete", "session_id", env.SessionID, "name", name, "request_id", env.ID, "targets", len(targets))
			_ = writeEnvelope(conn, reply)
		case protocol.TypeTerminalInput:
			_, name, ok := protocol.SplitSessionID(env.SessionID)
			if !ok {
				name = payloadName(env)
			}
			var input protocol.TerminalInput
			_ = env.DecodePayload(&input)
			w.Logger.Debug("terminal input received", "session_id", env.SessionID, "stream_id", env.StreamID, "name", name, "bytes", len(input.Data))
			if w.writeTerminal(env.StreamID, input.Data) {
				continue
			}
			w.Logger.Debug("terminal input fallback to backend", "session_id", env.SessionID, "stream_id", env.StreamID, "name", name, "bytes", len(input.Data))
			if err := w.Backend.SendTerminalInput(ctx, name, input.Data); err != nil {
				w.Logger.Debug("terminal input failed", "session_id", env.SessionID, "stream_id", env.StreamID, "name", name, "error", err)
				w.sendError(conn, env.SessionID, err.Error())
			}
		case protocol.TypeTerminalOpen:
			_, name, ok := protocol.SplitSessionID(env.SessionID)
			if !ok {
				name = payloadName(env)
			}
			var open protocol.TerminalOpen
			if err := env.DecodePayload(&open); err != nil {
				w.sendStreamError(conn, env.StreamID, env.SessionID, err.Error())
				continue
			}
			size := open.Size()
			w.Logger.Debug("terminal open received", "session_id", env.SessionID, "stream_id", env.StreamID, "name", name, "cols", size.Cols, "rows", size.Rows, "pane_id", protocolTargetPane(open.Target))
			w.startStream(ctx, conn, env.StreamID, protocol.SessionID(w.ID, name), name, size, open.Target)
		case protocol.TypeTerminalResize:
			var size protocol.TerminalSize
			if err := env.DecodePayload(&size); err != nil {
				w.sendStreamError(conn, env.StreamID, env.SessionID, err.Error())
				continue
			}
			w.Logger.Debug("terminal resize received", "session_id", env.SessionID, "stream_id", env.StreamID, "cols", size.Cols, "rows", size.Rows)
			if err := w.resizeTerminal(env.StreamID, size); err != nil {
				w.Logger.Debug("terminal resize failed", "session_id", env.SessionID, "stream_id", env.StreamID, "error", err)
				w.sendStreamError(conn, env.StreamID, env.SessionID, err.Error())
			}
		case protocol.TypeTerminalClose:
			w.Logger.Debug("terminal close received", "session_id", env.SessionID, "stream_id", env.StreamID)
			w.stopStream(env.StreamID)
		case protocol.TypeWorkerUpdateApply:
			w.handleUpdateApply(conn, env)
		}
	}
}

func (w *Worker) handleUpdateApply(conn *ws.Conn, env protocol.Envelope) {
	var req protocol.WorkerUpdateApply
	if err := env.DecodePayload(&req); err != nil {
		w.sendUpdateResult(conn, env.ID, protocol.WorkerUpdateResult{JobID: firstNonEmpty(req.JobID, env.ID), Status: "failed", Message: err.Error()})
		return
	}
	if req.JobID == "" {
		req.JobID = env.ID
	}
	if req.Version == "" {
		req.Version = "latest"
	}
	if req.Role == "" {
		req.Role = "worker"
	}
	if req.Role != "worker" {
		w.sendUpdateResult(conn, env.ID, protocol.WorkerUpdateResult{JobID: req.JobID, Status: "rejected", Version: req.Version, Message: "worker can only apply worker updates"})
		return
	}
	backend := strings.ToLower(w.Backend.Name())
	if backend != "tmux" && !req.AllowDisruptiveRestart {
		w.sendUpdateResult(conn, env.ID, protocol.WorkerUpdateResult{JobID: req.JobID, Status: "rejected", Version: req.Version, Message: "worker update requires disruptive restart confirmation for backend " + backend})
		return
	}
	executable, err := os.Executable()
	if err != nil {
		w.sendUpdateResult(conn, env.ID, protocol.WorkerUpdateResult{JobID: req.JobID, Status: "failed", Version: req.Version, Message: err.Error()})
		return
	}
	args := []string{"update", "apply", "--role", "worker", "--version", req.Version, "--path", executable}
	if req.Repo != "" {
		args = append(args, "--repo", req.Repo)
	}
	serviceBackend := strings.ToLower(w.Software.ServiceBackend)
	if supervisedWorkerService(serviceBackend) {
		w.Logger.Info("worker supervised update started", "job", req.JobID, "version", req.Version, "service_backend", serviceBackend)
		w.sendUpdateResult(conn, env.ID, protocol.WorkerUpdateResult{JobID: req.JobID, Status: "started", Version: req.Version, Message: "staging update"})
		go func() {
			cmd := exec.Command(executable, args...)
			cmd.Env = append(os.Environ(), "AGENTMUX_UPDATE_JOB_ID="+req.JobID)
			output, err := cmd.CombinedOutput()
			if err != nil {
				w.Logger.Error("worker supervised update failed", "job", req.JobID, "error", err, "output", strings.TrimSpace(string(output)))
				w.sendUpdateResult(conn, env.ID, protocol.WorkerUpdateResult{JobID: req.JobID, Status: "failed", Version: req.Version, Message: compactUpdateOutput(err, output)})
				return
			}
			w.Logger.Info("worker supervised update staged", "job", req.JobID, "version", req.Version)
			w.sendUpdateResult(conn, env.ID, protocol.WorkerUpdateResult{JobID: req.JobID, Status: "restarting", Version: req.Version, Message: "update staged; worker exiting for service restart"})
			time.Sleep(250 * time.Millisecond)
			os.Exit(0)
		}()
		return
	}
	if req.Restart {
		args = append(args, "--restart")
	}
	cmd := exec.Command(executable, args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), "AGENTMUX_UPDATE_JOB_ID="+req.JobID)
	if err := cmd.Start(); err != nil {
		w.sendUpdateResult(conn, env.ID, protocol.WorkerUpdateResult{JobID: req.JobID, Status: "failed", Version: req.Version, Message: err.Error()})
		return
	}
	w.Logger.Info("worker update started", "job", req.JobID, "version", req.Version, "pid", cmd.Process.Pid)
	w.sendUpdateResult(conn, env.ID, protocol.WorkerUpdateResult{JobID: req.JobID, Status: "started", Version: req.Version, Message: fmt.Sprintf("pid=%d", cmd.Process.Pid)})
	go func() {
		if err := cmd.Wait(); err != nil {
			w.Logger.Error("worker update process failed", "job", req.JobID, "error", err)
			return
		}
		w.Logger.Info("worker update process exited", "job", req.JobID)
	}()
}

func supervisedWorkerService(serviceBackend string) bool {
	switch serviceBackend {
	case "systemd-user", "launchd":
		return true
	default:
		return false
	}
}

func compactUpdateOutput(err error, output []byte) string {
	text := strings.TrimSpace(string(output))
	if text == "" {
		return err.Error()
	}
	if len(text) > 600 {
		text = text[:600] + "..."
	}
	return err.Error() + ": " + text
}

func (w *Worker) sendUpdateResult(conn *ws.Conn, requestID string, result protocol.WorkerUpdateResult) {
	reply, _ := protocol.NewEnvelope(protocol.TypeWorkerUpdateResult, result)
	reply.ID = requestID
	reply.WorkerID = w.ID
	_ = writeEnvelope(conn, reply)
}

func (w *Worker) sendSnapshot(ctx context.Context, conn *ws.Conn) error {
	sessions, err := w.Backend.List(ctx)
	if err != nil {
		return err
	}
	payload := protocol.SessionSnapshot{Sessions: make([]protocol.Session, 0, len(sessions))}
	for _, session := range sessions {
		payload.Sessions = append(payload.Sessions, protocol.Session{
			Name: session.Name, CWD: session.CWD, Command: session.Command, Status: session.Status, Backend: w.Backend.Name(),
		})
	}
	env, err := protocol.NewEnvelope(protocol.TypeSessionSnapshot, payload)
	if err != nil {
		return err
	}
	env.WorkerID = w.ID
	w.Logger.Debug("session snapshot sent", "sessions", len(payload.Sessions))
	return writeEnvelope(conn, env)
}

func (w *Worker) startStream(parent context.Context, conn *ws.Conn, streamID, sessionID, name string, size protocol.TerminalSize, target *protocol.TerminalTarget) {
	if name == "" || streamID == "" {
		w.Logger.Debug("terminal open ignored", "session_id", sessionID, "stream_id", streamID, "name", name)
		return
	}
	ctx, cancel := context.WithCancel(parent)
	w.Logger.Debug("terminal backend open start", "session_id", sessionID, "stream_id", streamID, "name", name, "cols", size.Cols, "rows", size.Rows, "pane_id", protocolTargetPane(target))
	terminal, err := w.openTerminalTarget(ctx, name, size, target)
	if err != nil {
		w.Logger.Debug("terminal backend open failed", "session_id", sessionID, "stream_id", streamID, "name", name, "error", err)
		w.sendStreamError(conn, streamID, sessionID, err.Error())
		cancel()
		return
	}
	w.mu.Lock()
	w.streams[streamID] = cancel
	w.terms[streamID] = terminal
	activeStreams := len(w.streams)
	w.mu.Unlock()
	w.Logger.Debug("terminal backend open complete", "session_id", sessionID, "stream_id", streamID, "name", name, "streams", activeStreams)
	go func() {
		defer func() {
			w.Logger.Debug("terminal read loop ended", "session_id", sessionID, "stream_id", streamID, "name", name)
			w.removeStream(streamID)
		}()
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
				if writeErr := writeEnvelope(conn, env); writeErr != nil {
					w.Logger.Debug("terminal output write failed", "session_id", sessionID, "stream_id", streamID, "name", name, "bytes", n, "error", writeErr)
					return
				}
				w.Logger.Debug("terminal output sent", "session_id", sessionID, "stream_id", streamID, "name", name, "bytes", n)
			}
			if err != nil {
				if err != io.EOF && ctx.Err() == nil && !strings.Contains(err.Error(), "input/output error") {
					w.Logger.Debug("terminal read error", "session_id", sessionID, "stream_id", streamID, "name", name, "error", err)
					w.sendStreamError(conn, streamID, sessionID, err.Error())
				}
				return
			}
		}
	}()
}

func (w *Worker) openTerminalTarget(ctx context.Context, name string, size protocol.TerminalSize, target *protocol.TerminalTarget) (sessionbackend.Stream, error) {
	if target != nil && target.PaneID != "" {
		backend, ok := w.Backend.(sessionbackend.TargetBackend)
		if !ok {
			return nil, fmt.Errorf("worker backend %s does not support pane targets", w.Backend.Name())
		}
		item := sessionbackend.TerminalTarget{
			SessionName:  firstNonEmpty(target.SessionName, name),
			WindowID:     target.WindowID,
			WindowIndex:  target.WindowIndex,
			WindowName:   target.WindowName,
			WindowActive: target.WindowActive,
			PaneID:       target.PaneID,
			PaneIndex:    target.PaneIndex,
			PaneActive:   target.PaneActive,
			CWD:          target.CWD,
			Command:      target.Command,
			Left:         target.Left,
			Top:          target.Top,
			Width:        target.Width,
			Height:       target.Height,
		}
		return backend.OpenTarget(ctx, item, size.Cols, size.Rows)
	}
	return w.Backend.Open(ctx, name, size.Cols, size.Rows)
}

func (w *Worker) removeStream(streamID string) {
	w.mu.Lock()
	cancel := w.streams[streamID]
	delete(w.streams, streamID)
	terminal := w.terms[streamID]
	delete(w.terms, streamID)
	activeStreams := len(w.streams)
	w.mu.Unlock()
	w.Logger.Debug("terminal stream removed", "stream_id", streamID, "streams", activeStreams, "had_terminal", terminal != nil)
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
		w.Logger.Debug("terminal write missed stream", "stream_id", streamID, "bytes", len(data))
		return false
	}
	_, err := terminal.Write([]byte(data))
	if err != nil {
		w.Logger.Debug("terminal write failed", "stream_id", streamID, "bytes", len(data), "error", err)
		return false
	}
	w.Logger.Debug("terminal write complete", "stream_id", streamID, "bytes", len(data))
	return err == nil
}

func (w *Worker) resizeTerminal(streamID string, size protocol.TerminalSize) error {
	w.mu.Lock()
	terminal := w.terms[streamID]
	w.mu.Unlock()
	if terminal == nil {
		w.Logger.Debug("terminal resize missed stream", "stream_id", streamID, "cols", size.Cols, "rows", size.Rows)
		return nil
	}
	err := terminal.Resize(size.Cols, size.Rows)
	if err != nil {
		w.Logger.Debug("terminal resize failed", "stream_id", streamID, "cols", size.Cols, "rows", size.Rows, "error", err)
		return err
	}
	w.Logger.Debug("terminal resize complete", "stream_id", streamID, "cols", size.Cols, "rows", size.Rows)
	return nil
}

func (w *Worker) stopStream(streamID string) {
	w.Logger.Debug("terminal stream stop requested", "stream_id", streamID)
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
	payload, err := ExchangeSignalDetail(ctx, hubURL, signal, deviceID, deviceName, "")
	if err != nil {
		return "", "", err
	}
	return payload.Credential, payload.DeviceID, nil
}

func ExchangeSignalDetail(ctx context.Context, hubURL, signal, deviceID, deviceName string, instanceID ...string) (ExchangedCredential, error) {
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
	if len(instanceID) > 0 && strings.TrimSpace(instanceID[0]) != "" {
		req["instance_id"] = strings.TrimSpace(instanceID[0])
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
		return ExchangedCredential{}, ExchangeHTTPError{
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
			Body:       strings.TrimSpace(string(data)),
		}
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

func protocolTargets(targets []sessionbackend.TerminalTarget) []protocol.TerminalTarget {
	result := make([]protocol.TerminalTarget, 0, len(targets))
	for _, target := range targets {
		result = append(result, protocol.TerminalTarget{
			SessionName:  target.SessionName,
			WindowID:     target.WindowID,
			WindowIndex:  target.WindowIndex,
			WindowName:   target.WindowName,
			WindowActive: target.WindowActive,
			PaneID:       target.PaneID,
			PaneIndex:    target.PaneIndex,
			PaneActive:   target.PaneActive,
			CWD:          target.CWD,
			Command:      target.Command,
			Left:         target.Left,
			Top:          target.Top,
			Width:        target.Width,
			Height:       target.Height,
		})
	}
	return result
}

func protocolTargetPane(target *protocol.TerminalTarget) string {
	if target == nil {
		return ""
	}
	return target.PaneID
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
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
