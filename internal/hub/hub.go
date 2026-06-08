package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net"
	"net/http"
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
        '<p><strong>Worker</strong></p>' +
        '<pre>' + escapeHTML(data.worker_command) + '</pre>' +
        '<p><strong>Control</strong></p>' +
        '<pre>' + escapeHTML(data.control_command) + '</pre>';
    });
    function escapeHTML(value) {
      return String(value).replace(/[&<>"']/g, ch => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[ch]));
    }
  </script>
</body>
</html>`))
