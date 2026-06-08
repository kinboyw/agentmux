package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"private/agentmux/internal/protocol"
	"private/agentmux/internal/ws"
)

type Server struct {
	addr   string
	token  string
	logger *slog.Logger

	mu          sync.RWMutex
	workers     map[string]*workerConn
	sessions    map[string]protocol.SessionView
	subscribers map[string]*controlConn
	joinTokens  *joinTokenStore
}

type workerConn struct {
	id       string
	name     string
	addr     string
	lastSeen time.Time
	conn     *ws.Conn
	send     chan protocol.Envelope
}

type controlConn struct {
	conn *ws.Conn
	send chan protocol.Envelope
}

func New(addr, token string, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		addr:        addr,
		token:       token,
		logger:      logger,
		workers:     map[string]*workerConn{},
		sessions:    map[string]protocol.SessionView{},
		subscribers: map[string]*controlConn{},
		joinTokens:  newJoinTokenStore(),
	}
}

func (s *Server) ListenAndServe(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleLanding)
	mux.HandleFunc("/control", s.handleControlPage)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/api/join-tokens", s.handleJoinTokens)
	mux.HandleFunc("/api/workers", s.requireAuth(s.handleWorkers))
	mux.HandleFunc("/api/sessions", s.requireAuth(s.handleSessions))
	mux.HandleFunc("/api/sessions/", s.requireAuth(s.handleSessionAction))
	mux.HandleFunc("/ws/worker", s.handleWorkerWS)
	mux.HandleFunc("/ws/control", s.handleControlWS)

	server := &http.Server{Addr: s.addr, Handler: mux}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	s.logger.Info("hub listening", "addr", s.addr)
	err := server.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleLanding(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	baseURL := requestBaseURL(r)
	data := landingData{
		BaseURL: baseURL,
		WSURL:   websocketBase(baseURL),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = landingTemplate.Execute(w, data)
}

func (s *Server) handleControlPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	baseURL := requestBaseURL(r)
	data := controlPageData{
		BaseURL: baseURL,
		WSURL:   websocketBase(baseURL),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = controlTemplate.Execute(w, data)
}

func (s *Server) handleJoinTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	minted, err := s.joinTokens.Mint(defaultJoinTokenTTL, 2, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	baseURL := requestBaseURL(r)
	writeJSON(w, http.StatusCreated, map[string]any{
		"token":           minted.Token,
		"expires_at":      minted.ExpiresAt,
		"uses_remaining":  minted.UsesRemaining,
		"reusable":        true,
		"scopes":          minted.Scopes,
		"worker_command":  installWorkerCommand(baseURL, minted.Token),
		"control_command": installControlCommand(baseURL, minted.Token),
		"control_url":     controlPageURL(baseURL, minted.Token),
	})
}

func (s *Server) handleWorkers(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	workers := make([]protocol.WorkerView, 0, len(s.workers))
	for _, worker := range s.workers {
		workers = append(workers, protocol.WorkerView{
			ID:       worker.id,
			Name:     worker.name,
			Addr:     worker.addr,
			LastSeen: worker.lastSeen,
		})
	}
	s.mu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{"workers": workers})
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.mu.RLock()
		sessions := make([]protocol.SessionView, 0, len(s.sessions))
		for _, session := range s.sessions {
			sessions = append(sessions, session)
		}
		s.mu.RUnlock()
		writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
	case http.MethodPost:
		var req protocol.CreateSession
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if strings.TrimSpace(req.WorkerID) == "" || strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.Command) == "" {
			writeError(w, http.StatusBadRequest, "worker_id, name and command are required")
			return
		}
		if err := s.sendToWorker(req.WorkerID, protocol.TypeSessionCreate, protocol.Session{
			Name: req.Name, CWD: req.CWD, Command: req.Command,
		}, "", ""); err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued"})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleSessionAction(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/sessions/"), "/")
	if len(parts) < 2 {
		writeError(w, http.StatusNotFound, "session path must be /api/sessions/{worker}/{name}")
		return
	}
	workerID, name := parts[0], parts[1]
	sessionID := protocol.SessionID(workerID, name)
	if r.Method == http.MethodDelete {
		if err := s.sendToWorker(workerID, protocol.TypeSessionKill, map[string]string{"name": name}, "", sessionID); err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued"})
		return
	}
	if r.Method == http.MethodPost && len(parts) == 3 && parts[2] == "input" {
		var input protocol.TerminalInput
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := s.sendToWorker(workerID, protocol.TypeTerminalInput, input, name, sessionID); err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued"})
		return
	}
	w.WriteHeader(http.StatusMethodNotAllowed)
}

func (s *Server) handleWorkerWS(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	conn, err := ws.Upgrade(w, r)
	if err != nil {
		s.logger.Error("worker websocket upgrade failed", "error", err)
		return
	}
	first, err := readEnvelope(conn)
	if err != nil || first.Type != protocol.TypeWorkerHello || first.WorkerID == "" {
		_ = conn.Close()
		return
	}
	var hello protocol.WorkerHello
	_ = first.DecodePayload(&hello)
	addr := remoteHost(r.RemoteAddr)
	worker := &workerConn{
		id:       first.WorkerID,
		name:     hello.Name,
		addr:     addr,
		lastSeen: time.Now().UTC(),
		conn:     conn,
		send:     make(chan protocol.Envelope, 64),
	}
	s.registerWorker(worker)
	defer s.unregisterWorker(worker.id)

	go writeLoop(conn, worker.send)
	for {
		env, err := readEnvelope(conn)
		if err != nil {
			return
		}
		s.handleWorkerMessage(worker, env)
	}
}

func (s *Server) handleControlWS(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	conn, err := ws.Upgrade(w, r)
	if err != nil {
		s.logger.Error("control websocket upgrade failed", "error", err)
		return
	}
	control := &controlConn{conn: conn, send: make(chan protocol.Envelope, 64)}
	defer s.removeControl(control)
	go writeLoop(conn, control.send)
	for {
		env, err := readEnvelope(conn)
		if err != nil {
			return
		}
		s.handleControlMessage(control, env)
	}
}

func (s *Server) registerWorker(worker *workerConn) {
	s.mu.Lock()
	old := s.workers[worker.id]
	if old != nil {
		_ = old.conn.Close()
	}
	s.workers[worker.id] = worker
	s.mu.Unlock()
	s.logger.Info("worker connected", "worker", worker.id)
}

func (s *Server) unregisterWorker(workerID string) {
	s.mu.Lock()
	delete(s.workers, workerID)
	for id := range s.sessions {
		if strings.HasPrefix(id, workerID+"/") {
			delete(s.sessions, id)
		}
	}
	s.mu.Unlock()
	s.logger.Info("worker disconnected", "worker", workerID)
}

func (s *Server) handleWorkerMessage(worker *workerConn, env protocol.Envelope) {
	s.mu.Lock()
	worker.lastSeen = time.Now().UTC()
	s.mu.Unlock()
	switch env.Type {
	case protocol.TypeWorkerHeartbeat:
		return
	case protocol.TypeSessionSnapshot:
		var snapshot protocol.SessionSnapshot
		if err := env.DecodePayload(&snapshot); err != nil {
			return
		}
		s.updateSessions(worker.id, snapshot.Sessions)
	case protocol.TypeTerminalOutput:
		s.publish(env.StreamID, env)
	case protocol.TypeError:
		s.publish(env.StreamID, env)
	}
}

func (s *Server) updateSessions(workerID string, sessions []protocol.Session) {
	s.mu.Lock()
	for id := range s.sessions {
		if strings.HasPrefix(id, workerID+"/") {
			delete(s.sessions, id)
		}
	}
	for _, session := range sessions {
		id := protocol.SessionID(workerID, session.Name)
		s.sessions[id] = protocol.SessionView{
			ID: id, WorkerID: workerID, Name: session.Name,
			CWD: session.CWD, Command: session.Command, Status: session.Status,
		}
	}
	s.mu.Unlock()
}

func (s *Server) handleControlMessage(control *controlConn, env protocol.Envelope) {
	switch env.Type {
	case protocol.TypeControlOpen:
		if env.SessionID == "" {
			sendError(control.send, "", "session_id is required")
			return
		}
		if env.StreamID == "" {
			sendError(control.send, env.SessionID, "stream_id is required")
			return
		}
		workerID, name, ok := protocol.SplitSessionID(env.SessionID)
		if !ok {
			sendError(control.send, env.SessionID, "invalid session_id")
			return
		}
		s.addSubscriber(env.StreamID, control)
		if err := s.sendToWorker(workerID, protocol.TypeTerminalOpen, map[string]string{"name": name}, name, env.SessionID, env.StreamID); err != nil {
			sendError(control.send, env.SessionID, err.Error())
		}
	case protocol.TypeControlInput:
		workerID, name, ok := protocol.SplitSessionID(env.SessionID)
		if !ok {
			sendError(control.send, env.SessionID, "invalid session_id")
			return
		}
		var input protocol.TerminalInput
		if err := env.DecodePayload(&input); err != nil {
			sendError(control.send, env.SessionID, err.Error())
			return
		}
		if err := s.sendToWorker(workerID, protocol.TypeTerminalInput, input, name, env.SessionID, env.StreamID); err != nil {
			sendError(control.send, env.SessionID, err.Error())
		}
	case protocol.TypeTerminalResize:
		workerID, name, ok := protocol.SplitSessionID(env.SessionID)
		if !ok {
			sendError(control.send, env.SessionID, "invalid session_id")
			return
		}
		var size protocol.TerminalSize
		if err := env.DecodePayload(&size); err != nil {
			sendError(control.send, env.SessionID, err.Error())
			return
		}
		if err := s.sendToWorker(workerID, protocol.TypeTerminalResize, size, name, env.SessionID, env.StreamID); err != nil {
			sendError(control.send, env.SessionID, err.Error())
		}
	case protocol.TypeTerminalClose:
		if env.StreamID == "" {
			return
		}
		workerID, name, ok := protocol.SplitSessionID(env.SessionID)
		if !ok {
			return
		}
		_ = s.sendToWorker(workerID, protocol.TypeTerminalClose, map[string]string{"name": name}, name, env.SessionID, env.StreamID)
	}
}

func (s *Server) addSubscriber(streamID string, control *controlConn) {
	s.mu.Lock()
	s.subscribers[streamID] = control
	s.mu.Unlock()
}

func (s *Server) removeControl(control *controlConn) {
	s.mu.Lock()
	for id, subscribed := range s.subscribers {
		if subscribed == control {
			delete(s.subscribers, id)
		}
	}
	s.mu.Unlock()
	_ = control.conn.Close()
}

func (s *Server) publish(streamID string, env protocol.Envelope) {
	s.mu.RLock()
	control := s.subscribers[streamID]
	s.mu.RUnlock()
	if control == nil {
		return
	}
	select {
	case control.send <- env:
	default:
	}
}

func (s *Server) sendToWorker(workerID, messageType string, payload any, name, sessionID string, streamID ...string) error {
	raw, err := protocol.MarshalPayload(payload)
	if err != nil {
		return err
	}
	s.mu.RLock()
	worker := s.workers[workerID]
	s.mu.RUnlock()
	if worker == nil {
		return fmt.Errorf("worker not connected: %s", workerID)
	}
	env := protocol.Envelope{Type: messageType, WorkerID: workerID, SessionID: sessionID, Payload: raw}
	if len(streamID) > 0 {
		env.StreamID = streamID[0]
	}
	if name != "" && sessionID == "" {
		env.SessionID = protocol.SessionID(workerID, name)
	}
	select {
	case worker.send <- env:
		return nil
	default:
		return fmt.Errorf("worker send queue full: %s", workerID)
	}
}

func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (s *Server) authorized(r *http.Request) bool {
	if s.token == "" {
		token := bearerOrQueryToken(r)
		return token == "" || s.joinTokens.Valid(token)
	}
	token := bearerOrQueryToken(r)
	if token == s.token {
		return true
	}
	return s.joinTokens.Valid(token)
}

func bearerOrQueryToken(r *http.Request) string {
	if token := r.URL.Query().Get("token"); token != "" {
		return token
	}
	header := r.Header.Get("Authorization")
	scheme, token, ok := strings.Cut(header, " ")
	if ok && strings.EqualFold(scheme, "Bearer") {
		return token
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

func writeLoop(conn *ws.Conn, send <-chan protocol.Envelope) {
	for env := range send {
		raw, err := json.Marshal(env)
		if err != nil {
			continue
		}
		if err := conn.WriteText(string(raw)); err != nil {
			return
		}
	}
}

func sendError(send chan<- protocol.Envelope, sessionID, message string) {
	raw, _ := protocol.MarshalPayload(protocol.ErrorPayload{Message: message})
	select {
	case send <- protocol.Envelope{Type: protocol.TypeError, SessionID: sessionID, Payload: raw}:
	default:
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func remoteHost(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}

type landingData struct {
	BaseURL string
	WSURL   string
}

type controlPageData struct {
	BaseURL string
	WSURL   string
}

func requestBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	host := r.Host
	if forwardedHost := r.Header.Get("X-Forwarded-Host"); forwardedHost != "" {
		host = forwardedHost
	}
	return scheme + "://" + host
}

func controlPageURL(baseURL string, token string) string {
	u, err := url.Parse(baseURL + "/control")
	if err != nil {
		return baseURL + "/control"
	}
	q := u.Query()
	q.Set("token", token)
	u.RawQuery = q.Encode()
	return u.String()
}

var landingTemplate = template.Must(template.New("landing").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>AgentMux Hub</title>
  <style>
    :root { color-scheme: light dark; font-family: ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; }
    body { margin: 0; background: Canvas; color: CanvasText; }
    main { max-width: 980px; margin: 0 auto; padding: 48px 24px; }
    h1 { font-size: 36px; margin: 0 0 12px; }
    p { color: color-mix(in srgb, CanvasText 70%, Canvas 30%); line-height: 1.5; }
    .panel { border: 1px solid color-mix(in srgb, CanvasText 18%, Canvas 82%); border-radius: 8px; padding: 20px; margin-top: 24px; }
    button { font: inherit; padding: 10px 14px; border-radius: 6px; border: 1px solid color-mix(in srgb, CanvasText 25%, Canvas 75%); background: CanvasText; color: Canvas; cursor: pointer; }
    code, pre { font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; }
    pre { overflow-x: auto; padding: 14px; border-radius: 6px; background: color-mix(in srgb, CanvasText 8%, Canvas 92%); }
    .muted { font-size: 14px; }
  </style>
</head>
<body>
  <main>
    <h1>AgentMux Hub</h1>
    <p>Generate a short-lived join signal, connect a worker, then control long-lived agent sessions through this hub.</p>
    <div class="panel">
      <p class="muted">Hub: <code>{{.BaseURL}}</code><br>Worker WebSocket: <code>{{.WSURL}}</code></p>
      <button id="mint">Generate join signal</button>
      <div id="result"></div>
    </div>
  </main>
  <script>
    const result = document.getElementById('result');
    document.getElementById('mint').addEventListener('click', async () => {
      result.textContent = 'Generating...';
      const res = await fetch('/api/join-tokens', { method: 'POST' });
      if (!res.ok) {
        result.textContent = 'Failed: ' + await res.text();
        return;
      }
      const data = await res.json();
      result.innerHTML =
        '<p><strong>Join token</strong><br><code>' + escapeHTML(data.token) + '</code></p>' +
        '<p class="muted">Expires at ' + escapeHTML(data.expires_at) + '. Prototype tokens are reusable until expiry.</p>' +
        '<p><strong>Web Control</strong><br><a href="' + escapeAttr(data.control_url) + '">' + escapeHTML(data.control_url) + '</a></p>' +
        '<p><strong>Worker</strong></p>' +
        '<pre>' + escapeHTML(data.worker_command) + '</pre>' +
        '<p><strong>Control</strong></p>' +
        '<pre>' + escapeHTML(data.control_command) + '</pre>';
    });
    function escapeHTML(value) {
      return String(value).replace(/[&<>"']/g, ch => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[ch]));
    }
    function escapeAttr(value) {
      return escapeHTML(value);
    }
  </script>
</body>
</html>`))

var controlTemplate = template.Must(template.New("control").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>AgentMux Control</title>
  <link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/@xterm/xterm/css/xterm.css">
  <style>
    :root {
      color-scheme: dark;
      --bg: #111315;
      --panel: #181b1f;
      --panel-2: #20252a;
      --line: #343a40;
      --text: #eef2f3;
      --muted: #a8b0b8;
      --accent: #35c98f;
      --warn: #f2b84b;
      --danger: #ff6b6b;
      font-family: ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
    }
    * { box-sizing: border-box; }
    html, body { height: 100%; }
    body { margin: 0; background: var(--bg); color: var(--text); overflow: hidden; }
    button, input, select { font: inherit; }
    button {
      min-height: 34px;
      border: 1px solid var(--line);
      border-radius: 6px;
      background: var(--panel-2);
      color: var(--text);
      padding: 7px 10px;
      cursor: pointer;
    }
    button.primary { background: var(--accent); border-color: var(--accent); color: #07130e; font-weight: 650; }
    button:disabled { cursor: not-allowed; opacity: .55; }
    input, select {
      min-height: 34px;
      width: 100%;
      border: 1px solid var(--line);
      border-radius: 6px;
      background: #0d0f11;
      color: var(--text);
      padding: 7px 9px;
    }
    .app { display: grid; grid-template-columns: 320px minmax(0, 1fr); height: 100vh; min-height: 0; }
    .sidebar { border-right: 1px solid var(--line); background: var(--panel); min-height: 0; display: flex; flex-direction: column; }
    .brand { padding: 14px 14px 12px; border-bottom: 1px solid var(--line); display: flex; align-items: center; justify-content: space-between; gap: 12px; }
    .brand h1 { margin: 0; font-size: 16px; line-height: 1.2; letter-spacing: 0; }
    .token { padding: 12px 14px; border-bottom: 1px solid var(--line); display: grid; gap: 8px; }
    .token label, .create label { font-size: 12px; color: var(--muted); }
    .sessions { min-height: 0; overflow: auto; padding: 8px; }
    .session {
      width: 100%;
      text-align: left;
      display: grid;
      gap: 3px;
      margin-bottom: 6px;
      background: transparent;
      border-color: transparent;
    }
    .session:hover, .session.active { background: var(--panel-2); border-color: var(--line); }
    .session strong { font-size: 13px; overflow-wrap: anywhere; }
    .session span { color: var(--muted); font-size: 12px; overflow-wrap: anywhere; }
    .create { border-top: 1px solid var(--line); padding: 12px 14px 14px; display: grid; gap: 8px; }
    .create .row { display: grid; gap: 6px; }
    .terminal-shell { min-width: 0; min-height: 0; display: grid; grid-template-rows: auto minmax(0, 1fr); }
    .status {
      min-height: 44px;
      border-bottom: 1px solid var(--line);
      background: #15181b;
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 12px;
      padding: 8px 12px;
    }
    .status-main { min-width: 0; display: grid; gap: 2px; }
    .status-title { font-size: 13px; font-weight: 650; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
    .status-sub { color: var(--muted); font-size: 12px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
    .status-actions { display: flex; align-items: center; gap: 8px; flex: none; }
    .badge { display: inline-flex; align-items: center; gap: 6px; color: var(--muted); font-size: 12px; }
    .dot { width: 8px; height: 8px; border-radius: 999px; background: var(--muted); }
    .dot.ok { background: var(--accent); }
    .dot.warn { background: var(--warn); }
    .dot.err { background: var(--danger); }
    .terminal-wrap { min-width: 0; min-height: 0; padding: 8px; background: #050607; }
    #terminal { width: 100%; height: 100%; }
    .empty {
      height: 100%;
      display: grid;
      place-items: center;
      color: var(--muted);
      border: 1px dashed #30363d;
      border-radius: 8px;
      font-size: 14px;
    }
    @media (max-width: 860px) {
      .app { grid-template-columns: 1fr; grid-template-rows: 42vh minmax(0, 1fr); }
      .sidebar { border-right: 0; border-bottom: 1px solid var(--line); }
      .create { display: none; }
    }
  </style>
</head>
<body>
  <div class="app">
    <aside class="sidebar">
      <div class="brand">
        <h1>AgentMux Control</h1>
        <button id="refresh" title="Refresh sessions">Refresh</button>
      </div>
      <div class="token">
        <label for="token">Token</label>
        <input id="token" autocomplete="off" spellcheck="false" placeholder="amx_join_... or shared token">
      </div>
      <div id="sessions" class="sessions"></div>
      <form id="create" class="create">
        <div class="row">
          <label for="worker">Worker</label>
          <select id="worker"></select>
        </div>
        <div class="row">
          <label for="name">Session</label>
          <input id="name" value="demo" autocomplete="off">
        </div>
        <div class="row">
          <label for="cwd">Working directory</label>
          <input id="cwd" value=".">
        </div>
        <div class="row">
          <label for="command">Command</label>
          <input id="command" value="bash">
        </div>
        <button class="primary" type="submit">Create Session</button>
      </form>
    </aside>
    <main class="terminal-shell">
      <header class="status">
        <div class="status-main">
          <div id="statusTitle" class="status-title">No session attached</div>
          <div id="statusSub" class="status-sub">Hub {{.BaseURL}}</div>
        </div>
        <div class="status-actions">
          <span class="badge"><span id="dot" class="dot"></span><span id="state">idle</span></span>
          <button id="detach" disabled>Detach</button>
        </div>
      </header>
      <section class="terminal-wrap">
        <div id="terminal"><div class="empty">Select or create a session.</div></div>
      </section>
    </main>
  </div>
  <script type="module">
    import { Terminal } from 'https://cdn.jsdelivr.net/npm/@xterm/xterm/+esm';
    import { FitAddon } from 'https://cdn.jsdelivr.net/npm/@xterm/addon-fit/+esm';

    const hubBase = '{{.BaseURL}}';
    const wsBase = '{{.WSURL}}';
    const tokenInput = document.getElementById('token');
    const sessionsEl = document.getElementById('sessions');
    const workerSelect = document.getElementById('worker');
    const terminalEl = document.getElementById('terminal');
    const statusTitle = document.getElementById('statusTitle');
    const statusSub = document.getElementById('statusSub');
    const stateEl = document.getElementById('state');
    const dotEl = document.getElementById('dot');
    const detachButton = document.getElementById('detach');

    let sessions = [];
    let workers = [];
    let activeSession = '';
    let term = null;
    let fit = null;
    let socket = null;
    let streamId = '';
    let resizeTimer = 0;

    const initialToken = new URLSearchParams(location.search).get('token') || localStorage.getItem('agentmux.token') || '';
    tokenInput.value = initialToken;
    if (initialToken) refreshAll();

    tokenInput.addEventListener('change', () => {
      localStorage.setItem('agentmux.token', tokenInput.value.trim());
      refreshAll();
    });
    document.getElementById('refresh').addEventListener('click', refreshAll);
    detachButton.addEventListener('click', detach);
    window.addEventListener('beforeunload', detach);
    document.addEventListener('keydown', capturePageKey, true);
    terminalEl.addEventListener('pointerdown', () => {
      if (term) term.focus();
    });

    document.getElementById('create').addEventListener('submit', async event => {
      event.preventDefault();
      const workerID = workerSelect.value;
      if (!workerID) {
        setStatus('No worker selected', 'Connect a worker before creating a session.', 'err');
        return;
      }
      const payload = {
        worker_id: workerID,
        name: document.getElementById('name').value.trim(),
        cwd: document.getElementById('cwd').value.trim() || '.',
        command: document.getElementById('command').value.trim() || 'bash'
      };
      const res = await apiFetch('/api/sessions', { method: 'POST', body: JSON.stringify(payload) });
      if (!res.ok) {
        setStatus('Create failed', await res.text(), 'err');
        return;
      }
      setStatus('Create queued', workerID + '/' + payload.name, 'warn');
      setTimeout(refreshAll, 700);
    });

    async function refreshAll() {
      localStorage.setItem('agentmux.token', tokenInput.value.trim());
      await Promise.all([loadWorkers(), loadSessions()]);
    }

    async function loadWorkers() {
      const res = await apiFetch('/api/workers');
      if (!res.ok) {
        workerSelect.innerHTML = '';
        setStatus('Unauthorized or hub unavailable', await res.text(), 'err');
        return;
      }
      const data = await res.json();
      workers = data.workers || [];
      workerSelect.innerHTML = workers.map(worker => '<option value="' + escapeAttr(worker.id) + '">' + escapeHTML(worker.id) + '</option>').join('');
    }

    async function loadSessions() {
      const res = await apiFetch('/api/sessions');
      if (!res.ok) {
        sessionsEl.innerHTML = '<div class="session"><span>' + escapeHTML(await res.text()) + '</span></div>';
        return;
      }
      const data = await res.json();
      sessions = data.sessions || [];
      renderSessions();
      if (!activeSession) {
        setStatus('No session attached', sessions.length ? 'Select a session from the left.' : 'No sessions reported by workers.', sessions.length ? 'warn' : '');
      }
    }

    function renderSessions() {
      if (!sessions.length) {
        sessionsEl.innerHTML = '<div class="session"><span>No sessions.</span></div>';
        return;
      }
      sessionsEl.innerHTML = sessions.map(session => {
        const active = session.id === activeSession ? ' active' : '';
        return '<button class="session' + active + '" data-id="' + escapeAttr(session.id) + '">' +
          '<strong>' + escapeHTML(session.id) + '</strong>' +
          '<span>' + escapeHTML(session.command || '') + ' · ' + escapeHTML(session.status || 'unknown') + '</span>' +
          '<span>' + escapeHTML(session.cwd || '') + '</span>' +
          '</button>';
      }).join('');
      sessionsEl.querySelectorAll('button[data-id]').forEach(button => {
        button.addEventListener('click', () => attach(button.dataset.id));
      });
    }

    async function attach(sessionID) {
      detach();
      activeSession = sessionID;
      renderSessions();
      terminalEl.innerHTML = '';
      term = new Terminal({
        cursorBlink: true,
        convertEol: false,
        scrollback: 5000,
        fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Consolas, monospace',
        fontSize: 14,
        theme: { background: '#050607', foreground: '#eef2f3', cursor: '#35c98f' }
      });
      fit = new FitAddon();
      term.loadAddon(fit);
      term.open(terminalEl);
      fit.fit();
      term.focus();
      streamId = makeStreamID(sessionID);
      const wsURL = wsBase + '/ws/control?token=' + encodeURIComponent(tokenInput.value.trim());
      socket = new WebSocket(wsURL);
      setStatus('Connecting ' + sessionID, 'Opening remote PTY stream.', 'warn');
      detachButton.disabled = false;

      socket.addEventListener('open', () => {
        sendEnvelope('control.open', sessionID, { cols: term.cols, rows: term.rows });
        setStatus('Attached ' + sessionID, 'stream ' + streamId, 'ok');
      });
      socket.addEventListener('message', event => handleMessage(event.data));
      socket.addEventListener('close', () => {
        detachButton.disabled = true;
        if (activeSession === sessionID) setStatus('Detached ' + sessionID, 'The browser control stream is closed.', 'warn');
      });
      socket.addEventListener('error', () => setStatus('WebSocket error', sessionID, 'err'));

      term.onData(data => sendEnvelope('control.input', sessionID, { data }));
      window.addEventListener('resize', scheduleResize);
      scheduleResize();
    }

    function handleMessage(raw) {
      let env;
      try {
        env = JSON.parse(raw);
      } catch {
        return;
      }
      if (env.type === 'terminal.output' && term) {
        const payload = env.payload || {};
        if (payload.encoding === 'base64') {
          term.write(base64ToBytes(payload.data || ''));
        } else {
          term.write(payload.data || '');
        }
      } else if (env.type === 'error') {
        const message = (env.payload && env.payload.message) || 'unknown error';
        setStatus('Remote error', message, 'err');
        if (term) term.write('\r\n[agentmux] ' + message + '\r\n');
      }
    }

    function scheduleResize() {
      if (!term || !fit) return;
      clearTimeout(resizeTimer);
      resizeTimer = setTimeout(() => {
        fit.fit();
        sendEnvelope('terminal.resize', activeSession, { cols: term.cols, rows: term.rows });
      }, 80);
    }

    function detach() {
      const previousSession = activeSession;
      window.removeEventListener('resize', scheduleResize);
      if (socket && socket.readyState === WebSocket.OPEN) {
        sendEnvelope('terminal.close', activeSession, {});
      }
      if (socket) socket.close();
      socket = null;
      streamId = '';
      detachButton.disabled = true;
      if (term) {
        term.dispose();
        term = null;
        fit = null;
      }
      activeSession = '';
      renderSessions();
      if (previousSession) setStatus('Detached ' + previousSession, 'The browser control stream is closed.', 'warn');
      terminalEl.innerHTML = '<div class="empty">Select or create a session.</div>';
    }

    function capturePageKey(event) {
      if (!term || !activeSession || event.defaultPrevented) return;
      if (!shouldCaptureKey(event)) return;
      if (isTerminalEvent(event) && !shouldManuallyCaptureTerminalKey(event)) return;
      const data = encodeKeyEvent(event);
      if (!data) return;
      event.preventDefault();
      event.stopPropagation();
      if (event.stopImmediatePropagation) event.stopImmediatePropagation();
      term.focus();
      sendEnvelope('control.input', activeSession, { data });
    }

    function isTerminalEvent(event) {
      return event.target instanceof Node && terminalEl.contains(event.target);
    }

    function shouldCaptureKey(event) {
      if (event.isComposing) return false;
      return !event.metaKey;
    }

    function shouldManuallyCaptureTerminalKey(event) {
      return event.ctrlKey || event.altKey || event.key === 'Tab';
    }

    function encodeKeyEvent(event) {
      const key = event.key;
      if (key === 'Enter') return '\r';
      if (key === 'Tab') return event.shiftKey ? '\x1b[Z' : '\t';
      if (key === 'Backspace') return '\x7f';
      if (key === 'Escape') return '\x1b';
      if (key === 'Delete') return '\x1b[3~';
      if (key === 'Insert') return '\x1b[2~';
      if (key === 'Home') return '\x1b[H';
      if (key === 'End') return '\x1b[F';
      if (key === 'PageUp') return '\x1b[5~';
      if (key === 'PageDown') return '\x1b[6~';
      if (key === 'ArrowUp') return '\x1b[A';
      if (key === 'ArrowDown') return '\x1b[B';
      if (key === 'ArrowRight') return '\x1b[C';
      if (key === 'ArrowLeft') return '\x1b[D';
      if (/^F([1-9]|1[0-2])$/.test(key)) return functionKeySequence(key);
      if (event.ctrlKey && !event.altKey) return controlSequence(key);
      if (event.altKey && key.length === 1) return '\x1b' + key;
      if (!event.ctrlKey && !event.altKey && key.length === 1) return key;
      return '';
    }

    function controlSequence(key) {
      if (key === ' ') return '\x00';
      if (key === '[') return '\x1b';
      if (key === '\\') return '\x1c';
      if (key === ']') return '\x1d';
      if (key === '^') return '\x1e';
      if (key === '_' || key === '-') return '\x1f';
      if (key.length === 1) {
        const code = key.toUpperCase().charCodeAt(0);
        if (code >= 65 && code <= 90) return String.fromCharCode(code - 64);
      }
      return '';
    }

    function functionKeySequence(key) {
      return ({
        F1: '\x1bOP',
        F2: '\x1bOQ',
        F3: '\x1bOR',
        F4: '\x1bOS',
        F5: '\x1b[15~',
        F6: '\x1b[17~',
        F7: '\x1b[18~',
        F8: '\x1b[19~',
        F9: '\x1b[20~',
        F10: '\x1b[21~',
        F11: '\x1b[23~',
        F12: '\x1b[24~'
      })[key] || '';
    }

    function sendEnvelope(type, sessionID, payload) {
      if (!socket || socket.readyState !== WebSocket.OPEN) return;
      socket.send(JSON.stringify({ type, session_id: sessionID, stream_id: streamId, payload }));
    }

    async function apiFetch(path, options = {}) {
      const headers = new Headers(options.headers || {});
      headers.set('Authorization', 'Bearer ' + tokenInput.value.trim());
      if (options.body && !headers.has('Content-Type')) headers.set('Content-Type', 'application/json');
      return fetch(hubBase + path, { ...options, headers });
    }

    function setStatus(title, subtitle, state) {
      statusTitle.textContent = title;
      statusSub.textContent = subtitle || ('Hub ' + hubBase);
      stateEl.textContent = state || 'idle';
      dotEl.className = 'dot' + (state ? ' ' + state : '');
    }

    function makeStreamID(sessionID) {
      const id = crypto.randomUUID ? crypto.randomUUID() : String(Math.random()).slice(2);
      return 'web-' + Date.now() + '-' + id + '|' + sessionID + '|';
    }

    function base64ToBytes(value) {
      const text = atob(value);
      const bytes = new Uint8Array(text.length);
      for (let i = 0; i < text.length; i++) bytes[i] = text.charCodeAt(i);
      return bytes;
    }

    function escapeHTML(value) {
      return String(value).replace(/[&<>"']/g, ch => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[ch]));
    }
    function escapeAttr(value) {
      return escapeHTML(value);
    }
  </script>
</body>
</html>`))
