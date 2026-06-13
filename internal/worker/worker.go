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
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"private/agentmux/internal/credentialcache"
	"private/agentmux/internal/protocol"
	"private/agentmux/internal/sessionbackend"
	"private/agentmux/internal/terminalview"
	"private/agentmux/internal/ws"
)

const (
	workerPingInterval = 15 * time.Second
	workerPongWait     = 45 * time.Second

	terminalHistoryMaxLines     = 2000
	terminalHistoryDefaultLimit = 200
	terminalHistoryMaxLimit     = 1000
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
	HubURL           string
	Token            string
	CredentialID     string
	TenantID         string
	DeviceID         string
	DeviceName       string
	Role             string
	ExpiresAt        time.Time
	RefreshToken     string
	RefreshExpiresAt time.Time
	Source           string
}

type ExchangedCredential struct {
	Credential       string    `json:"credential"`
	CredentialID     string    `json:"credential_id"`
	TenantID         string    `json:"tenant_id"`
	Role             string    `json:"role"`
	DeviceID         string    `json:"device_id"`
	ExpiresAt        time.Time `json:"expires_at"`
	Scopes           []string  `json:"scopes"`
	RefreshToken     string    `json:"refresh_token,omitempty"`
	RefreshExpiresAt time.Time `json:"refresh_expires_at,omitempty"`
}

type RefreshedCredential struct {
	Credential       string    `json:"credential"`
	CredentialID     string    `json:"credential_id"`
	TenantID         string    `json:"tenant_id"`
	Role             string    `json:"role"`
	DeviceID         string    `json:"device_id"`
	ExpiresAt        time.Time `json:"expires_at"`
	Scopes           []string  `json:"scopes"`
	RefreshToken     string    `json:"refresh_token,omitempty"`
	RefreshExpiresAt time.Time `json:"refresh_expires_at,omitempty"`
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
	HubURL       string
	Token        string
	ID           string
	InstanceID   string
	Name         string
	Version      string
	Software     protocol.WorkerSoftware
	Backend      sessionbackend.Backend
	TerminalMode string
	StateSize    protocol.TerminalSize
	Logger       *slog.Logger
	Interval     time.Duration

	mu                sync.Mutex
	streams           map[string]context.CancelFunc
	terms             map[string]sessionbackend.Stream
	streamModes       map[string]string
	streamRenderModes map[string]string
	streamStateKeys   map[string]string
	streamStates      map[string]*terminalStateStream
	streamTargets     map[string]*protocol.TerminalTarget
	streamCellUpdates map[string]bool
	generations       map[string]int
	histories         map[string]*terminalHistoryBuffer
	terminalSeq       int64

	credentialMu    sync.Mutex
	credentialEntry credentialcache.Entry
}

type terminalHistoryBuffer struct {
	lines []protocol.TerminalHistoryLine
}

type terminalStateStream struct {
	view         *terminalview.View
	size         protocol.TerminalSize
	lastSnapshot time.Time
	lastCells    *protocol.TerminalCellSnapshot
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
		Backend:  backend, TerminalMode: "auto", StateSize: protocol.TerminalSize{Cols: 120, Rows: 36},
		Logger: logger, Interval: time.Second,
		streams:           map[string]context.CancelFunc{},
		terms:             map[string]sessionbackend.Stream{},
		streamModes:       map[string]string{},
		streamRenderModes: map[string]string{},
		streamStateKeys:   map[string]string{},
		streamStates:      map[string]*terminalStateStream{},
		streamTargets:     map[string]*protocol.TerminalTarget{},
		streamCellUpdates: map[string]bool{},
		generations:       map[string]int{},
		histories:         map[string]*terminalHistoryBuffer{},
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
			DeviceName: deviceName, ExpiresAt: credential.ExpiresAt, RefreshToken: credential.RefreshToken,
			RefreshExpiresAt: credential.RefreshExpiresAt, UpdatedAt: time.Now().UTC(),
		}
		_ = credentialcache.Save(entry)
		return AuthResult{
			HubURL: hubURL, Token: credential.Credential, CredentialID: credential.CredentialID,
			TenantID: credential.TenantID, DeviceID: credential.DeviceID, DeviceName: deviceName,
			Role: credential.Role, ExpiresAt: credential.ExpiresAt, RefreshToken: credential.RefreshToken,
			RefreshExpiresAt: credential.RefreshExpiresAt, Source: "join",
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
		Role: entry.Role, ExpiresAt: entry.ExpiresAt, RefreshToken: entry.RefreshToken,
		RefreshExpiresAt: entry.RefreshExpiresAt, Source: "cache",
	}, nil
}

func (w *Worker) WithCredentialEntry(entry credentialcache.Entry) *Worker {
	w.credentialMu.Lock()
	defer w.credentialMu.Unlock()
	w.credentialEntry = entry
	return w
}

func (w *Worker) Run(ctx context.Context) error {
	defer w.closeBackend()
	backoff := 2 * time.Second
	for {
		if err := w.EnsureFreshCredential(ctx); err != nil {
			w.Logger.Warn("worker credential refresh failed; retrying with current credential", "error", err)
		}
		token := w.currentToken()
		target, err := workerURL(w.HubURL, token)
		if err != nil {
			return err
		}
		if err := w.runOnce(ctx, target); err != nil && ctx.Err() == nil {
			if isAuthHandshakeError(err) {
				if refreshErr := w.refreshCredential(ctx); refreshErr != nil {
					w.Logger.Error("worker credential refresh after auth failure failed", "error", refreshErr)
				} else {
					backoff = 2 * time.Second
					continue
				}
			}
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

func (w *Worker) currentToken() string {
	w.credentialMu.Lock()
	defer w.credentialMu.Unlock()
	if w.credentialEntry.Credential != "" {
		return w.credentialEntry.Credential
	}
	return w.Token
}

func (w *Worker) EnsureFreshCredential(ctx context.Context) error {
	w.credentialMu.Lock()
	entry := w.credentialEntry
	w.credentialMu.Unlock()
	if !shouldRefreshCredential(entry) {
		return nil
	}
	return w.refreshCredential(ctx)
}

func shouldRefreshCredential(entry credentialcache.Entry) bool {
	if entry.RefreshToken == "" {
		return false
	}
	if entry.Credential == "" || entry.ExpiresAt.IsZero() {
		return true
	}
	return time.Until(entry.ExpiresAt) <= 2*time.Minute
}

func (w *Worker) refreshCredential(ctx context.Context) error {
	w.credentialMu.Lock()
	entry := w.credentialEntry
	w.credentialMu.Unlock()
	if entry.RefreshToken == "" {
		return fmt.Errorf("no worker refresh token available")
	}
	if !entry.RefreshExpiresAt.IsZero() && time.Now().UTC().After(entry.RefreshExpiresAt) {
		return fmt.Errorf("worker refresh token expired")
	}
	refreshed, err := RefreshCredential(ctx, entry.HubURL, entry.RefreshToken)
	if err != nil {
		return err
	}
	if refreshed.Role != "" && refreshed.Role != "worker" {
		return fmt.Errorf("refresh returned %q credential, expected worker", refreshed.Role)
	}
	next := credentialcache.Entry{
		HubURL: entry.HubURL, Credential: refreshed.Credential, CredentialID: refreshed.CredentialID,
		TenantID: refreshed.TenantID, Role: refreshed.Role, DeviceID: refreshed.DeviceID,
		DeviceName: entry.DeviceName, ExpiresAt: refreshed.ExpiresAt, RefreshToken: refreshed.RefreshToken,
		RefreshExpiresAt: refreshed.RefreshExpiresAt, UpdatedAt: time.Now().UTC(),
	}
	if next.Role == "" {
		next.Role = "worker"
	}
	if next.DeviceName == "" {
		next.DeviceName = refreshed.DeviceID
	}
	if err := credentialcache.Save(next); err != nil {
		return err
	}
	w.credentialMu.Lock()
	w.credentialEntry = next
	w.Token = next.Credential
	w.credentialMu.Unlock()
	w.Logger.Info("worker credential refreshed", "device_id", next.DeviceID, "expires_at", next.ExpiresAt)
	return nil
}

func isAuthHandshakeError(err error) bool {
	var handshake ws.HandshakeError
	if errors.As(err, &handshake) {
		return handshake.StatusCode == http.StatusUnauthorized || handshake.StatusCode == http.StatusForbidden
	}
	return false
}

func (w *Worker) runOnce(ctx context.Context, target string) error {
	conn, err := ws.Dial(ctx, target, w.currentToken())
	if err != nil {
		return err
	}
	defer conn.Close()
	defer w.stopAllStreams()
	software := w.Software
	if software.Version == "" {
		software.Version = w.Version
	}
	hello, _ := protocol.NewEnvelope(protocol.TypeWorkerHello, protocol.WorkerHello{Name: w.Name, Backend: w.Backend.Name(), InstanceID: w.InstanceID, WorkerSoftware: software})
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
		case protocol.TypeError:
			var payload protocol.ErrorPayload
			_ = env.DecodePayload(&payload)
			if payload.Message == "" {
				payload.Message = "hub returned an error"
			}
			return fmt.Errorf("hub error: %s", payload.Message)
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
			scope := "active_pane"
			w.Logger.Debug("preview capture start", "session_id", env.SessionID, "name", name, "request_id", env.ID, "lines", req.Lines, "pane_id", protocolTargetPane(req.Target))
			var data string
			var err error
			if req.Target != nil && req.Target.PaneID != "" {
				backend, ok := w.Backend.(sessionbackend.TargetCaptureBackend)
				if !ok {
					err = fmt.Errorf("worker backend %s does not support pane target preview", w.Backend.Name())
				} else {
					scope = "pane"
					data, err = backend.CaptureTarget(ctx, sessionBackendTarget(name, req.Target), req.Lines)
				}
			} else {
				data, err = w.Backend.Capture(ctx, name, req.Lines)
			}
			if err != nil {
				w.Logger.Debug("preview capture failed", "session_id", env.SessionID, "name", name, "request_id", env.ID, "error", err)
				w.sendRequestError(conn, env.ID, env.SessionID, err.Error())
				continue
			}
			w.Logger.Debug("preview capture complete", "session_id", env.SessionID, "name", name, "request_id", env.ID, "bytes", len(data))
			reply, _ := protocol.NewEnvelope(protocol.TypeSessionPreview, protocol.SessionPreview{Data: data, Scope: scope})
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
			w.startStream(ctx, conn, env.StreamID, protocol.SessionID(w.ID, name), name, size, open)
		case protocol.TypeTerminalResize:
			var size protocol.TerminalSize
			if err := env.DecodePayload(&size); err != nil {
				w.sendStreamError(conn, env.StreamID, env.SessionID, err.Error())
				continue
			}
			w.Logger.Debug("terminal resize received", "session_id", env.SessionID, "stream_id", env.StreamID, "cols", size.Cols, "rows", size.Rows)
			if !w.streamUsesAttachResize(env.StreamID) {
				w.Logger.Debug("terminal viewport resize ignored in worker state mode", "session_id", env.SessionID, "stream_id", env.StreamID, "cols", size.Cols, "rows", size.Rows)
				continue
			}
			if err := w.resizeTerminal(env.StreamID, size); err != nil {
				w.Logger.Debug("terminal resize failed", "session_id", env.SessionID, "stream_id", env.StreamID, "error", err)
				w.sendStreamError(conn, env.StreamID, env.SessionID, err.Error())
			}
		case protocol.TypeTerminalSizeSync:
			_, name, ok := protocol.SplitSessionID(env.SessionID)
			if !ok {
				name = payloadName(env)
			}
			var req protocol.TerminalSizeSync
			if err := env.DecodePayload(&req); err != nil {
				w.sendStreamError(conn, env.StreamID, env.SessionID, err.Error())
				continue
			}
			size := protocol.TerminalSize{Cols: req.Cols, Rows: req.Rows}
			if err := w.applyTerminalRemoteSize(ctx, conn, env.StreamID, env.SessionID, name, size, "size_sync"); err != nil {
				w.sendStreamError(conn, env.StreamID, env.SessionID, err.Error())
			}
		case protocol.TypeTerminalSizeReset:
			_, name, ok := protocol.SplitSessionID(env.SessionID)
			if !ok {
				name = payloadName(env)
			}
			size := w.defaultStateSize()
			if err := w.applyTerminalRemoteSize(ctx, conn, env.StreamID, env.SessionID, name, size, "size_reset"); err != nil {
				w.sendStreamError(conn, env.StreamID, env.SessionID, err.Error())
			}
		case protocol.TypeTerminalHistoryReq:
			var req protocol.TerminalHistoryRequest
			_ = env.DecodePayload(&req)
			reply, _ := protocol.NewEnvelope(protocol.TypeTerminalHistoryPage, w.historyPage(w.stateKeyForStream(env.StreamID, env.SessionID), req))
			reply.WorkerID = w.ID
			reply.SessionID = env.SessionID
			reply.StreamID = env.StreamID
			_ = writeEnvelope(conn, reply)
		case protocol.TypeTerminalMouse:
			var mouse protocol.TerminalMouse
			if err := env.DecodePayload(&mouse); err != nil {
				w.sendStreamError(conn, env.StreamID, env.SessionID, err.Error())
				continue
			}
			if !w.writeTerminalMouse(env.StreamID, mouse) {
				w.Logger.Debug("terminal mouse ignored", "session_id", env.SessionID, "stream_id", env.StreamID, "x", mouse.X, "y", mouse.Y, "button", mouse.Button)
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

func (w *Worker) startStream(parent context.Context, conn *ws.Conn, streamID, sessionID, name string, size protocol.TerminalSize, open protocol.TerminalOpen) {
	if name == "" || streamID == "" {
		w.Logger.Debug("terminal open ignored", "session_id", sessionID, "stream_id", streamID, "name", name)
		return
	}
	ctx, cancel := context.WithCancel(parent)
	mode := w.effectiveTerminalMode(open)
	renderMode := w.effectiveRenderMode(open, mode)
	sendCells := renderMode == protocol.RenderModeWorkerStateXterm && terminalOpenRequestsCapability(open, "terminal.cells.v1")
	remoteSize := size
	if mode != "attach" {
		remoteSize = w.defaultStateSize()
	}
	w.sendTerminalMode(conn, streamID, sessionID, protocol.TerminalMode{
		Mode: mode, RenderMode: renderMode, Capabilities: w.terminalModeCapabilities(mode, renderMode, sendCells), ResizePolicy: w.resizePolicyForMode(mode),
		RemoteSize: remoteSize, ViewportSize: size, DefaultSize: w.defaultStateSize(),
	})
	if mode != "attach" && terminalOpenRequestsCapability(open, "terminal.snapshot.v1") {
		w.sendTerminalSnapshot(ctx, conn, streamID, sessionID, name, remoteSize, open.Target)
	}
	w.Logger.Debug("terminal backend open start", "session_id", sessionID, "stream_id", streamID, "name", name, "mode", mode, "cols", remoteSize.Cols, "rows", remoteSize.Rows, "pane_id", protocolTargetPane(open.Target))
	terminal, err := w.openTerminalTarget(ctx, name, remoteSize, open.Target)
	if err != nil {
		w.Logger.Debug("terminal backend open failed", "session_id", sessionID, "stream_id", streamID, "name", name, "error", err)
		w.sendStreamError(conn, streamID, sessionID, err.Error())
		cancel()
		return
	}
	w.mu.Lock()
	w.streams[streamID] = cancel
	w.terms[streamID] = terminal
	w.streamModes[streamID] = mode
	w.streamRenderModes[streamID] = renderMode
	w.streamCellUpdates[streamID] = sendCells
	if mode != "attach" {
		stateKey := terminalStateKey(sessionID, open.Target)
		w.streamStateKeys[streamID] = stateKey
		if state := w.streamStates[stateKey]; state == nil || state.view == nil {
			w.streamStates[stateKey] = &terminalStateStream{view: terminalview.New(remoteSize.Cols, remoteSize.Rows), size: remoteSize}
		}
	}
	w.streamTargets[streamID] = cloneTerminalTarget(open.Target)
	activeStreams := len(w.streams)
	w.mu.Unlock()
	w.Logger.Debug("terminal backend open complete", "session_id", sessionID, "stream_id", streamID, "name", name, "mode", mode, "render_mode", renderMode, "cells", sendCells, "streams", activeStreams)
	go func() {
		defer func() {
			w.Logger.Debug("terminal read loop ended", "session_id", sessionID, "stream_id", streamID, "name", name)
			w.removeStream(streamID)
		}()
		buffer := make([]byte, 8192)
		for {
			n, err := terminal.Read(buffer)
			if n > 0 {
				w.recordTerminalOutput(w.stateKeyForStream(streamID, sessionID), buffer[:n])
				w.recordTerminalState(streamID, buffer[:n])
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
				w.maybeSendTerminalCellSnapshot(conn, streamID, sessionID, "live")
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

func (w *Worker) sendTerminalSnapshot(ctx context.Context, conn *ws.Conn, streamID, sessionID, name string, size protocol.TerminalSize, target *protocol.TerminalTarget) {
	if data, cells, ok := w.stateANSIForTarget(sessionID, target); ok {
		generation := w.currentGeneration(terminalStateKey(sessionID, target))
		snapshot, _ := protocol.NewEnvelope(protocol.TypeTerminalSnapshot, protocol.TerminalSnapshot{
			Generation: generation, Seq: time.Now().UnixNano(), Cols: cells.Cols, Rows: cells.Rows,
			Encoding: "ansi-screen-v1", Data: data, Scope: "state", GeneratedAt: time.Now().UTC(),
		})
		snapshot.WorkerID = w.ID
		snapshot.SessionID = sessionID
		snapshot.StreamID = streamID
		if err := writeEnvelope(conn, snapshot); err != nil {
			w.Logger.Debug("terminal state snapshot write failed", "session_id", sessionID, "stream_id", streamID, "name", name, "error", err)
			return
		}
		w.Logger.Debug("terminal state snapshot sent", "session_id", sessionID, "stream_id", streamID, "name", name, "bytes", len(data), "cols", cells.Cols, "rows", cells.Rows)
		return
	}

	lines := size.Rows
	if lines <= 0 {
		lines = 80
	}
	scope := "active_pane"
	var data string
	var err error
	if target != nil && target.PaneID != "" {
		backend, ok := w.Backend.(sessionbackend.TargetCaptureBackend)
		if !ok {
			err = fmt.Errorf("worker backend %s does not support pane target snapshot", w.Backend.Name())
		} else {
			scope = "pane"
			data, err = backend.CaptureTarget(ctx, sessionBackendTarget(name, target), lines)
		}
	} else {
		data, err = w.Backend.Capture(ctx, name, lines)
	}
	if err != nil {
		w.Logger.Debug("terminal snapshot unavailable", "session_id", sessionID, "stream_id", streamID, "name", name, "error", err)
		w.sendTerminalMode(conn, streamID, sessionID, protocol.TerminalMode{Mode: "raw-pty"})
		return
	}
	generation := w.currentGeneration(terminalStateKey(sessionID, target))
	snapshot, _ := protocol.NewEnvelope(protocol.TypeTerminalSnapshot, protocol.TerminalSnapshot{
		Generation:  generation,
		Seq:         time.Now().UnixNano(),
		Cols:        size.Cols,
		Rows:        size.Rows,
		Encoding:    "ansi-lines-v1",
		Data:        data,
		Scope:       scope,
		GeneratedAt: time.Now().UTC(),
	})
	snapshot.WorkerID = w.ID
	snapshot.SessionID = sessionID
	snapshot.StreamID = streamID
	if err := writeEnvelope(conn, snapshot); err != nil {
		w.Logger.Debug("terminal snapshot write failed", "session_id", sessionID, "stream_id", streamID, "name", name, "error", err)
		return
	}
	w.Logger.Debug("terminal snapshot sent", "session_id", sessionID, "stream_id", streamID, "name", name, "bytes", len(data))
}

func (w *Worker) stateANSIForTarget(sessionID string, target *protocol.TerminalTarget) (string, terminalview.CellSnapshot, bool) {
	w.mu.Lock()
	state := w.streamStates[terminalStateKey(sessionID, target)]
	if state == nil || state.view == nil {
		w.mu.Unlock()
		return "", terminalview.CellSnapshot{}, false
	}
	cells := state.view.Cells()
	data := terminalview.SnapshotANSI(cells)
	w.mu.Unlock()
	return data, cells, true
}

func (w *Worker) recordTerminalState(streamID string, data []byte) {
	if len(data) == 0 {
		return
	}
	w.mu.Lock()
	state := w.stateForStreamLocked(streamID)
	if state != nil && state.view != nil {
		state.view.Write(data)
	}
	w.mu.Unlock()
}

func (w *Worker) resizeTerminalState(streamID string, size protocol.TerminalSize) {
	w.mu.Lock()
	state := w.stateForStreamLocked(streamID)
	if state != nil && state.view != nil {
		state.view.Resize(size.Cols, size.Rows)
		state.size = size
		state.lastSnapshot = time.Time{}
		state.lastCells = nil
	}
	w.mu.Unlock()
}

func (w *Worker) streamCellSnapshot(streamID string) (protocol.TerminalCellSnapshot, bool) {
	w.mu.Lock()
	state := w.stateForStreamLocked(streamID)
	if state == nil || state.view == nil {
		w.mu.Unlock()
		return protocol.TerminalCellSnapshot{}, false
	}
	snapshot := terminalCellSnapshot(state.view.Cells())
	w.mu.Unlock()
	return snapshot, true
}

func (w *Worker) maybeSendTerminalCellSnapshot(conn *ws.Conn, streamID, sessionID, scope string) {
	w.mu.Lock()
	if !w.streamCellUpdates[streamID] {
		w.mu.Unlock()
		return
	}
	state := w.stateForStreamLocked(streamID)
	if state == nil || state.view == nil {
		w.mu.Unlock()
		return
	}
	now := time.Now()
	if !state.lastSnapshot.IsZero() && now.Sub(state.lastSnapshot) < 120*time.Millisecond {
		w.mu.Unlock()
		return
	}
	state.lastSnapshot = now
	snapshot := terminalCellSnapshot(state.view.Cells())
	previous := state.lastCells
	state.lastCells = cloneTerminalCellSnapshot(snapshot)
	generation := w.generations[w.stateKeyForStreamLocked(streamID, sessionID)]
	if generation <= 0 {
		generation = 1
		w.generations[w.stateKeyForStreamLocked(streamID, sessionID)] = generation
	}
	w.mu.Unlock()
	if previous != nil {
		if diff, ok := terminalCellDiff(generation, *previous, snapshot); ok {
			w.sendTerminalDiff(conn, streamID, sessionID, diff)
			return
		}
	}
	w.sendTerminalCellSnapshot(conn, streamID, sessionID, generation, snapshot, scope)
}

func terminalCellDiff(generation int, previous, current protocol.TerminalCellSnapshot) (protocol.TerminalDiff, bool) {
	if previous.Cols != current.Cols || previous.Rows != current.Rows || len(previous.Lines) != len(current.Lines) {
		return protocol.TerminalDiff{}, false
	}
	ops := make([]protocol.TerminalDiffOp, 0)
	for row := range current.Lines {
		if row >= len(previous.Lines) || !reflect.DeepEqual(previous.Lines[row], current.Lines[row]) {
			ops = append(ops, protocol.TerminalDiffOp{Op: "replace_row", Row: row, Cells: current.Lines[row]})
		}
	}
	if !reflect.DeepEqual(previous.Cursor, current.Cursor) {
		cursor := current.Cursor
		ops = append(ops, protocol.TerminalDiffOp{Op: "cursor", Cursor: &cursor})
	}
	if len(ops) == 0 {
		return protocol.TerminalDiff{}, true
	}
	return protocol.TerminalDiff{
		Generation: generation,
		ToSeq:      time.Now().UnixNano(),
		Ops:        ops,
	}, true
}

func cloneTerminalCellSnapshot(snapshot protocol.TerminalCellSnapshot) *protocol.TerminalCellSnapshot {
	lines := make([][]protocol.TerminalCell, len(snapshot.Lines))
	for i, line := range snapshot.Lines {
		lines[i] = append([]protocol.TerminalCell(nil), line...)
	}
	copy := snapshot
	copy.Lines = lines
	return &copy
}

func (w *Worker) sendTerminalDiff(conn *ws.Conn, streamID, sessionID string, diff protocol.TerminalDiff) {
	if len(diff.Ops) == 0 {
		return
	}
	env, _ := protocol.NewEnvelope(protocol.TypeTerminalDiff, diff)
	env.WorkerID = w.ID
	env.SessionID = sessionID
	env.StreamID = streamID
	if err := writeEnvelope(conn, env); err != nil {
		w.Logger.Debug("terminal diff write failed", "session_id", sessionID, "stream_id", streamID, "error", err)
		return
	}
	w.Logger.Debug("terminal diff sent", "session_id", sessionID, "stream_id", streamID, "ops", len(diff.Ops))
}

func (w *Worker) sendTerminalCellSnapshot(conn *ws.Conn, streamID, sessionID string, generation int, cells protocol.TerminalCellSnapshot, scope string) {
	env, _ := protocol.NewEnvelope(protocol.TypeTerminalSnapshot, protocol.TerminalSnapshot{
		Generation:  generation,
		Seq:         time.Now().UnixNano(),
		Cols:        cells.Cols,
		Rows:        cells.Rows,
		Encoding:    "cells-v1",
		Cells:       &cells,
		Scope:       scope,
		GeneratedAt: time.Now().UTC(),
	})
	env.WorkerID = w.ID
	env.SessionID = sessionID
	env.StreamID = streamID
	if err := writeEnvelope(conn, env); err != nil {
		w.Logger.Debug("terminal cell snapshot write failed", "session_id", sessionID, "stream_id", streamID, "error", err)
		return
	}
	w.Logger.Debug("terminal cell snapshot sent", "session_id", sessionID, "stream_id", streamID, "cols", cells.Cols, "rows", cells.Rows)
}

func terminalCellSnapshot(snapshot terminalview.CellSnapshot) protocol.TerminalCellSnapshot {
	lines := make([][]protocol.TerminalCell, len(snapshot.Lines))
	for y, line := range snapshot.Lines {
		lines[y] = make([]protocol.TerminalCell, len(line))
		for x, cell := range line {
			lines[y][x] = protocol.TerminalCell{
				Text:           cell.Text,
				Width:          cell.Width,
				Fg:             cell.Fg,
				Bg:             cell.Bg,
				Bold:           cell.Bold,
				Faint:          cell.Faint,
				Italic:         cell.Italic,
				Blink:          cell.Blink,
				Reverse:        cell.Reverse,
				Conceal:        cell.Conceal,
				Strikethrough:  cell.Strikethrough,
				Underline:      cell.Underline,
				UnderlineColor: cell.UnderlineColor,
				Link:           cell.Link,
			}
		}
	}
	return protocol.TerminalCellSnapshot{
		Version: "cells-v1",
		Cols:    snapshot.Cols,
		Rows:    snapshot.Rows,
		Cursor: protocol.TerminalCursor{
			X:       snapshot.Cursor.X,
			Y:       snapshot.Cursor.Y,
			Visible: snapshot.Cursor.Visible,
		},
		Lines: lines,
	}
}

func (w *Worker) applyTerminalRemoteSize(ctx context.Context, conn *ws.Conn, streamID, sessionID, name string, size protocol.TerminalSize, reason string) error {
	if size.Cols <= 0 || size.Rows <= 0 {
		return fmt.Errorf("invalid terminal size %dx%d", size.Cols, size.Rows)
	}
	if err := w.resizeTerminal(streamID, size); err != nil {
		return err
	}
	w.resizeTerminalState(streamID, size)
	stateKey := w.stateKeyForStream(streamID, sessionID)
	generation := w.nextGeneration(stateKey)
	w.recordTerminalBoundary(stateKey, generation, fmt.Sprintf("--- %s: %dx%d ---", reason, size.Cols, size.Rows), "resize_boundary")
	reset, _ := protocol.NewEnvelope(protocol.TypeTerminalStateReset, protocol.TerminalStateReset{
		Generation: generation, Reason: reason, Cols: size.Cols, Rows: size.Rows,
		RemoteSize: size, DefaultSize: w.defaultStateSize(), GeneratedAt: time.Now().UTC(),
	})
	reset.WorkerID = w.ID
	reset.SessionID = sessionID
	reset.StreamID = streamID
	if err := writeEnvelope(conn, reset); err != nil {
		return err
	}
	renderMode := w.streamRenderMode(streamID)
	sendCells := w.streamWantsCellUpdates(streamID)
	w.sendTerminalMode(conn, streamID, sessionID, protocol.TerminalMode{
		Mode: "state-bridge", RenderMode: renderMode, Capabilities: w.terminalModeCapabilities("state-bridge", renderMode, sendCells), ResizePolicy: "worker_state",
		RemoteSize: size, DefaultSize: w.defaultStateSize(),
	})
	w.sendTerminalSnapshot(ctx, conn, streamID, sessionID, name, size, w.streamTarget(streamID))
	if sendCells {
		if cells, ok := w.streamCellSnapshot(streamID); ok {
			w.sendTerminalCellSnapshot(conn, streamID, sessionID, generation, cells, "resize")
		}
	}
	return nil
}

func (w *Worker) recordTerminalOutput(sessionID string, data []byte) {
	lines := terminalOutputLines(data)
	if len(lines) == 0 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	history := w.historyForLocked(sessionID)
	generation := w.generations[sessionID]
	if generation <= 0 {
		generation = 1
		w.generations[sessionID] = generation
	}
	for _, line := range lines {
		w.terminalSeq++
		seq := w.terminalSeq
		history.lines = append(history.lines, protocol.TerminalHistoryLine{
			SeqStart:   seq,
			SeqEnd:     seq,
			Generation: generation,
			Text:       line,
		})
	}
	trimTerminalHistory(history)
}

func (w *Worker) recordTerminalBoundary(sessionID string, generation int, text string, flags ...string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	history := w.historyForLocked(sessionID)
	w.terminalSeq++
	seq := w.terminalSeq
	history.lines = append(history.lines, protocol.TerminalHistoryLine{
		SeqStart:   seq,
		SeqEnd:     seq,
		Generation: generation,
		Text:       text,
		Flags:      append([]string(nil), flags...),
	})
	trimTerminalHistory(history)
}

func (w *Worker) historyPage(sessionID string, req protocol.TerminalHistoryRequest) protocol.TerminalHistoryPage {
	limit := req.LimitLines
	if limit <= 0 {
		limit = terminalHistoryDefaultLimit
	}
	if limit > terminalHistoryMaxLimit {
		limit = terminalHistoryMaxLimit
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	history := w.histories[sessionID]
	if history == nil || len(history.lines) == 0 {
		return protocol.TerminalHistoryPage{Lines: []protocol.TerminalHistoryLine{}}
	}
	end := len(history.lines)
	if req.BeforeSeq > 0 {
		for end > 0 && history.lines[end-1].SeqStart >= req.BeforeSeq {
			end--
		}
	}
	if end < 0 {
		end = 0
	}
	start := end - limit
	if start < 0 {
		start = 0
	}
	lines := append([]protocol.TerminalHistoryLine(nil), history.lines[start:end]...)
	page := protocol.TerminalHistoryPage{Lines: lines, HasMore: start > 0}
	if len(lines) > 0 {
		page.StartSeq = lines[0].SeqStart
		page.EndSeq = lines[len(lines)-1].SeqEnd
	}
	return page
}

func (w *Worker) historyForLocked(sessionID string) *terminalHistoryBuffer {
	history := w.histories[sessionID]
	if history == nil {
		history = &terminalHistoryBuffer{}
		w.histories[sessionID] = history
	}
	return history
}

func trimTerminalHistory(history *terminalHistoryBuffer) {
	if len(history.lines) <= terminalHistoryMaxLines {
		return
	}
	copy(history.lines, history.lines[len(history.lines)-terminalHistoryMaxLines:])
	history.lines = history.lines[:terminalHistoryMaxLines]
}

func terminalOutputLines(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	parts := strings.Split(text, "\n")
	lines := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimRight(part, "\x00")
		if part == "" {
			continue
		}
		lines = append(lines, part)
	}
	return lines
}

func (w *Worker) effectiveTerminalMode(open protocol.TerminalOpen) string {
	configured := strings.ToLower(strings.TrimSpace(w.TerminalMode))
	requested := strings.ToLower(strings.TrimSpace(open.TransportMode))
	renderMode := strings.ToLower(strings.TrimSpace(open.RenderMode))
	if configured == "" {
		configured = "auto"
	}
	if requested == "" && renderMode == "" {
		return "attach"
	}
	if configured == "attach" || requested == "attach" || renderMode == protocol.RenderModeLiveAttachXterm {
		return "attach"
	}
	if !terminalOpenRequestsCapability(open, "terminal.snapshot.v1") {
		return "attach"
	}
	if configured == "state" || requested == "state" || requested == "auto" || renderMode == protocol.RenderModeWorkerStateXterm || renderMode == protocol.RenderModeAuto {
		return "state-bridge"
	}
	return "attach"
}

func (w *Worker) effectiveRenderMode(open protocol.TerminalOpen, mode string) string {
	requested := strings.ToLower(strings.TrimSpace(open.RenderMode))
	switch requested {
	case protocol.RenderModeWorkerStateXterm:
		if mode != "attach" {
			return protocol.RenderModeWorkerStateXterm
		}
	case protocol.RenderModeLiveAttachXterm:
		return protocol.RenderModeLiveAttachXterm
	case protocol.RenderModeAuto, "":
		if mode != "attach" {
			return protocol.RenderModeWorkerStateXterm
		}
	}
	return protocol.RenderModeLiveAttachXterm
}

func (w *Worker) resizePolicyForMode(mode string) string {
	if mode == "attach" {
		return "follow_control"
	}
	return "worker_state"
}

func (w *Worker) terminalModeCapabilities(mode, renderMode string, includeCells bool) []string {
	if mode == "attach" {
		return []string{"terminal.open", "terminal.resize", "terminal.render.live_attach_xterm.v1"}
	}
	capabilities := []string{"terminal.snapshot.v1", "terminal.state_reset.v1", "terminal.size_control.v1", "terminal.history.v1", "terminal.render.worker_state_xterm.v1"}
	if includeCells {
		capabilities = append(capabilities, "terminal.cells.v1", "terminal.mouse.v1")
	}
	return capabilities
}

func (w *Worker) defaultStateSize() protocol.TerminalSize {
	size := w.StateSize
	if size.Cols <= 0 {
		size.Cols = 120
	}
	if size.Rows <= 0 {
		size.Rows = 36
	}
	return size
}

func (w *Worker) streamUsesAttachResize(streamID string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.streamModes[streamID] == "attach"
}

func (w *Worker) streamTarget(streamID string) *protocol.TerminalTarget {
	w.mu.Lock()
	defer w.mu.Unlock()
	return cloneTerminalTarget(w.streamTargets[streamID])
}

func (w *Worker) streamRenderMode(streamID string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	mode := w.streamRenderModes[streamID]
	if mode == "" {
		return protocol.RenderModeLiveAttachXterm
	}
	return mode
}

func (w *Worker) streamWantsCellUpdates(streamID string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.streamCellUpdates[streamID]
}

func (w *Worker) stateForStreamLocked(streamID string) *terminalStateStream {
	return w.streamStates[w.stateKeyForStreamLocked(streamID, streamID)]
}

func (w *Worker) stateKeyForStream(streamID, fallback string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stateKeyForStreamLocked(streamID, fallback)
}

func (w *Worker) stateKeyForStreamLocked(streamID, fallback string) string {
	if key := w.streamStateKeys[streamID]; key != "" {
		return key
	}
	return fallback
}

func cloneTerminalTarget(target *protocol.TerminalTarget) *protocol.TerminalTarget {
	if target == nil {
		return nil
	}
	copy := *target
	return &copy
}

func (w *Worker) currentGeneration(sessionID string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.generations[sessionID] <= 0 {
		w.generations[sessionID] = 1
	}
	return w.generations[sessionID]
}

func (w *Worker) nextGeneration(sessionID string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.generations[sessionID] <= 0 {
		w.generations[sessionID] = 1
	}
	w.generations[sessionID]++
	return w.generations[sessionID]
}

func (w *Worker) sendTerminalMode(conn *ws.Conn, streamID, sessionID string, mode protocol.TerminalMode) {
	env, _ := protocol.NewEnvelope(protocol.TypeTerminalMode, mode)
	env.WorkerID = w.ID
	env.SessionID = sessionID
	env.StreamID = streamID
	if err := writeEnvelope(conn, env); err != nil {
		w.Logger.Debug("terminal mode write failed", "session_id", sessionID, "stream_id", streamID, "mode", mode.Mode, "error", err)
	}
}

func terminalOpenRequestsCapability(open protocol.TerminalOpen, capability string) bool {
	for _, item := range open.Capabilities {
		if item == capability {
			return true
		}
	}
	return false
}

func (w *Worker) openTerminalTarget(ctx context.Context, name string, size protocol.TerminalSize, target *protocol.TerminalTarget) (sessionbackend.Stream, error) {
	if target != nil && target.PaneID != "" {
		backend, ok := w.Backend.(sessionbackend.TargetBackend)
		if !ok {
			return nil, fmt.Errorf("worker backend %s does not support pane targets", w.Backend.Name())
		}
		return backend.OpenTarget(ctx, sessionBackendTarget(name, target), size.Cols, size.Rows)
	}
	return w.Backend.Open(ctx, name, size.Cols, size.Rows)
}

func sessionBackendTarget(name string, target *protocol.TerminalTarget) sessionbackend.TerminalTarget {
	if target == nil {
		return sessionbackend.TerminalTarget{SessionName: name}
	}
	return sessionbackend.TerminalTarget{
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
}

func (w *Worker) removeStream(streamID string) {
	w.mu.Lock()
	cancel := w.streams[streamID]
	delete(w.streams, streamID)
	terminal := w.terms[streamID]
	delete(w.terms, streamID)
	delete(w.streamModes, streamID)
	delete(w.streamRenderModes, streamID)
	delete(w.streamStateKeys, streamID)
	delete(w.streamTargets, streamID)
	delete(w.streamCellUpdates, streamID)
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

func (w *Worker) writeTerminalMouse(streamID string, mouse protocol.TerminalMouse) bool {
	w.mu.Lock()
	state := w.stateForStreamLocked(streamID)
	if state == nil || state.view == nil {
		w.mu.Unlock()
		return false
	}
	input := state.view.MouseInput(terminalview.MouseEvent{
		X:       mouse.X,
		Y:       mouse.Y,
		Button:  protocolMouseButton(mouse.Button),
		Motion:  mouse.Motion,
		Release: mouse.Release,
		Shift:   mouse.Shift,
		Alt:     mouse.Alt,
		Ctrl:    mouse.Ctrl,
	})
	w.mu.Unlock()
	if input == "" {
		return false
	}
	return w.writeTerminal(streamID, input)
}

func protocolMouseButton(button string) terminalview.MouseButton {
	switch strings.ToLower(strings.TrimSpace(button)) {
	case "left":
		return terminalview.MouseLeft
	case "middle":
		return terminalview.MouseMiddle
	case "right":
		return terminalview.MouseRight
	case "wheel_up":
		return terminalview.MouseWheelUp
	case "wheel_down":
		return terminalview.MouseWheelDown
	case "wheel_left":
		return terminalview.MouseWheelLeft
	case "wheel_right":
		return terminalview.MouseWheelRight
	case "backward":
		return terminalview.MouseBackward
	case "forward":
		return terminalview.MouseForward
	default:
		return terminalview.MouseNone
	}
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
	w.mu.Lock()
	delete(w.histories, sessionID)
	delete(w.generations, sessionID)
	for key := range w.histories {
		if terminalStateKeyMatchesSession(key, sessionID) {
			delete(w.histories, key)
		}
	}
	for key := range w.generations {
		if terminalStateKeyMatchesSession(key, sessionID) {
			delete(w.generations, key)
		}
	}
	for key := range w.streamStates {
		if terminalStateKeyMatchesSession(key, sessionID) {
			delete(w.streamStates, key)
		}
	}
	w.mu.Unlock()
}

func (w *Worker) stopAllStreams() {
	w.mu.Lock()
	streams := w.streams
	w.streams = map[string]context.CancelFunc{}
	terms := w.terms
	w.terms = map[string]sessionbackend.Stream{}
	w.streamModes = map[string]string{}
	w.streamStateKeys = map[string]string{}
	w.streamStates = map[string]*terminalStateStream{}
	w.streamTargets = map[string]*protocol.TerminalTarget{}
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

func RefreshCredential(ctx context.Context, hubURL, refreshToken string) (RefreshedCredential, error) {
	base, err := httpBaseURL(hubURL)
	if err != nil {
		return RefreshedCredential{}, err
	}
	req := map[string]string{"refresh_token": refreshToken}
	raw, err := json.Marshal(req)
	if err != nil {
		return RefreshedCredential{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/auth/refresh", bytes.NewReader(raw))
	if err != nil {
		return RefreshedCredential{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return RefreshedCredential{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return RefreshedCredential{}, ExchangeHTTPError{
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
			Body:       strings.TrimSpace(string(data)),
		}
	}
	var payload RefreshedCredential
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return RefreshedCredential{}, err
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

func terminalStateKey(sessionID string, target *protocol.TerminalTarget) string {
	parts := []string{strings.TrimSpace(sessionID)}
	if target != nil {
		if target.SessionName != "" {
			parts = append(parts, "session="+target.SessionName)
		}
		if target.WindowID != "" {
			parts = append(parts, "window_id="+target.WindowID)
		} else if target.WindowIndex != 0 || target.WindowName != "" {
			parts = append(parts, "window_index="+strconv.Itoa(target.WindowIndex))
			if target.WindowName != "" {
				parts = append(parts, "window_name="+target.WindowName)
			}
		}
		if target.PaneID != "" {
			parts = append(parts, "pane_id="+target.PaneID)
		} else if target.PaneIndex != 0 {
			parts = append(parts, "pane_index="+strconv.Itoa(target.PaneIndex))
		}
	}
	return strings.Join(parts, "|")
}

func terminalStateKeyMatchesSession(key, sessionID string) bool {
	return key == sessionID || strings.HasPrefix(key, sessionID+"|")
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
