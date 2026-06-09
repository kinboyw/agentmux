package hub

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"private/agentmux/internal/protocol"
	"private/agentmux/internal/ws"
)

//go:embed webdist
var webDist embed.FS

type Server struct {
	addr        string
	token       string
	publicURL   string
	releaseRepo string
	logger      *slog.Logger

	mu          sync.RWMutex
	workers     map[string]*workerConn
	sessions    map[string]protocol.SessionView
	subscribers map[string]*controlConn
	previews    map[string]chan protocol.Envelope
	auth        AuthStore
}

type workerConn struct {
	id       string
	tenantID string
	name     string
	addr     string
	lastSeen time.Time
	conn     *ws.Conn
	send     chan protocol.Envelope
}

type controlConn struct {
	conn     *ws.Conn
	send     chan protocol.Envelope
	tenantID string
	admin    bool
}

type authContext struct {
	Admin      bool
	Credential credentialEntry
}

type authContextKey struct{}

func New(addr, token string, logger *slog.Logger) *Server {
	server, err := NewWithOptions(ServerOptions{Addr: addr, Token: token, Logger: logger})
	if err != nil {
		panic(err)
	}
	return server
}

type ServerOptions struct {
	Addr        string
	Token       string
	PublicURL   string
	ReleaseRepo string
	Logger      *slog.Logger
	AuthStore   AuthStore
}

func NewWithOptions(options ServerOptions) (*Server, error) {
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		addr:        options.Addr,
		token:       options.Token,
		publicURL:   strings.TrimRight(options.PublicURL, "/"),
		releaseRepo: defaultReleaseRepo(options.ReleaseRepo),
		logger:      logger,
		workers:     map[string]*workerConn{},
		sessions:    map[string]protocol.SessionView{},
		subscribers: map[string]*controlConn{},
		previews:    map[string]chan protocol.Envelope{},
		auth:        defaultAuthStore(options.AuthStore),
	}, nil
}

func defaultAuthStore(store AuthStore) AuthStore {
	if store != nil {
		return store
	}
	return newAuthStore()
}

func defaultReleaseRepo(repo string) string {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return "kinboyw/agentmux"
	}
	return repo
}

func (s *Server) ListenAndServe(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleLanding)
	mux.HandleFunc("/install.sh", s.handleInstallScript)
	mux.HandleFunc("/control", s.handleControlPage)
	mux.HandleFunc("/assets/", s.handleWebAssets)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/api/signals", s.handleSignals)
	mux.HandleFunc("/api/join-tokens", s.handleJoinTokens)
	mux.HandleFunc("/api/exchange", s.handleExchange)
	mux.HandleFunc("/api/auth/register", s.handleAuthRegister)
	mux.HandleFunc("/api/auth/login", s.handleAuthLogin)
	mux.HandleFunc("/api/auth/oauth/", s.handleAuthOAuth)
	mux.HandleFunc("/api/workers", s.requireRole("control", s.handleWorkers))
	mux.HandleFunc("/api/sessions", s.requireRole("control", s.handleSessions))
	mux.HandleFunc("/api/sessions/", s.requireRole("control", s.handleSessionAction))
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
	baseURL := s.requestBaseURL(r)
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
	if serveEmbeddedControl(w, r) {
		return
	}
	baseURL := s.requestBaseURL(r)
	data := controlPageData{
		BaseURL: baseURL,
		WSURL:   websocketBase(baseURL),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = controlTemplate.Execute(w, data)
}

func (s *Server) handleWebAssets(w http.ResponseWriter, r *http.Request) {
	dist, err := fs.Sub(webDist, "webdist")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	http.FileServer(http.FS(dist)).ServeHTTP(w, r)
}

func (s *Server) handleInstallScript(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	baseURL := s.requestBaseURL(r)
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	_, _ = fmt.Fprintf(w, installScriptTemplate, s.releaseRepo, baseURL, websocketBase(baseURL))
}

func (s *Server) handleJoinTokens(w http.ResponseWriter, r *http.Request) {
	s.handleSignals(w, r)
}

func (s *Server) handleSignals(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	minted, err := s.auth.MintSignal(defaultSignalTTL, 0, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	baseURL := s.requestBaseURL(r)
	writeJSON(w, http.StatusCreated, map[string]any{
		"token":           minted.Signal,
		"signal":          minted.Signal,
		"signal_id":       minted.ID,
		"tenant_id":       minted.TenantID,
		"expires_at":      minted.ExpiresAt,
		"uses_remaining":  minted.UsesRemaining,
		"reusable":        minted.UsesRemaining < 0,
		"scopes":          minted.Scopes,
		"worker_command":  installWorkerCommand(baseURL, minted.Signal),
		"control_command": installControlCommand(baseURL, minted.Signal),
		"control_url":     controlPageURL(baseURL, minted.Signal),
	})
}

func (s *Server) handleExchange(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req exchangeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	credential, err := s.auth.Exchange(req)
	if err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, credential)
}

func (s *Server) handleAuthRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req registerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	credential, err := s.auth.Register(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, credential)
}

func (s *Server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	credential, err := s.auth.Login(req)
	if err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, credential)
}

func (s *Server) handleAuthOAuth(w http.ResponseWriter, r *http.Request) {
	provider := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/auth/oauth/"), "/")
	if provider != "github" && provider != "google" {
		writeError(w, http.StatusNotFound, "unsupported oauth provider")
		return
	}
	writeJSON(w, http.StatusNotImplemented, map[string]string{
		"error":    "oauth provider is not configured",
		"provider": provider,
	})
}

func (s *Server) handleWorkers(w http.ResponseWriter, r *http.Request) {
	auth := requestAuth(r)
	s.mu.RLock()
	workers := make([]protocol.WorkerView, 0, len(s.workers))
	for _, worker := range s.workers {
		if !auth.Admin && worker.tenantID != auth.Credential.TenantID {
			continue
		}
		workers = append(workers, protocol.WorkerView{
			ID:       worker.id,
			TenantID: worker.tenantID,
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
		auth := requestAuth(r)
		s.mu.RLock()
		sessions := make([]protocol.SessionView, 0, len(s.sessions))
		for _, session := range s.sessions {
			if !auth.Admin && session.TenantID != auth.Credential.TenantID {
				continue
			}
			sessions = append(sessions, session)
		}
		s.mu.RUnlock()
		writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
	case http.MethodPost:
		auth := requestAuth(r)
		var req protocol.CreateSession
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if strings.TrimSpace(req.WorkerID) == "" || strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.Command) == "" {
			writeError(w, http.StatusBadRequest, "worker_id, name and command are required")
			return
		}
		if !auth.Admin && !s.workerInTenant(req.WorkerID, auth.Credential.TenantID) {
			writeError(w, http.StatusForbidden, "worker is not in credential tenant")
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
	auth := requestAuth(r)
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/sessions/"), "/")
	if len(parts) < 2 {
		writeError(w, http.StatusNotFound, "session path must be /api/sessions/{worker}/{name}")
		return
	}
	workerID, name := parts[0], parts[1]
	sessionID := protocol.SessionID(workerID, name)
	if !auth.Admin && !s.workerInTenant(workerID, auth.Credential.TenantID) {
		writeError(w, http.StatusForbidden, "worker is not in credential tenant")
		return
	}
	if r.Method == http.MethodDelete {
		if err := s.sendToWorker(workerID, protocol.TypeSessionKill, map[string]string{"name": name}, "", sessionID); err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued"})
		return
	}
	if r.Method == http.MethodGet && len(parts) == 3 && parts[2] == "preview" {
		lines := safeQueryInt(r.URL.Query().Get("lines"), 80)
		preview, err := s.requestSessionPreview(r.Context(), workerID, name, sessionID, lines)
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, preview)
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
	auth, ok := s.authenticateRole(r, "worker")
	if !ok {
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
		tenantID: authTenantID(auth),
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
	auth, ok := s.authenticateRole(r, "control")
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	conn, err := ws.Upgrade(w, r)
	if err != nil {
		s.logger.Error("control websocket upgrade failed", "error", err)
		return
	}
	control := &controlConn{conn: conn, send: make(chan protocol.Envelope, 64), tenantID: auth.Credential.TenantID, admin: auth.Admin}
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
	case protocol.TypeSessionPreview:
		s.completePreview(env)
	case protocol.TypeError:
		if env.ID != "" && s.completePreview(env) {
			return
		}
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
		tenantID := s.workerTenantIDLocked(workerID)
		s.sessions[id] = protocol.SessionView{
			ID: id, TenantID: tenantID, WorkerID: workerID, Name: session.Name,
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
		if !control.admin && !s.workerInTenant(workerID, control.tenantID) {
			sendError(control.send, env.SessionID, "worker is not in credential tenant")
			return
		}
		var size protocol.TerminalSize
		if err := env.DecodePayload(&size); err != nil {
			sendError(control.send, env.SessionID, err.Error())
			return
		}
		s.addSubscriber(env.StreamID, control)
		if err := s.sendToWorker(workerID, protocol.TypeTerminalOpen, size, name, env.SessionID, env.StreamID); err != nil {
			sendError(control.send, env.SessionID, err.Error())
		}
	case protocol.TypeControlInput:
		workerID, name, ok := protocol.SplitSessionID(env.SessionID)
		if !ok {
			sendError(control.send, env.SessionID, "invalid session_id")
			return
		}
		if !control.admin && !s.workerInTenant(workerID, control.tenantID) {
			sendError(control.send, env.SessionID, "worker is not in credential tenant")
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
		if !control.admin && !s.workerInTenant(workerID, control.tenantID) {
			sendError(control.send, env.SessionID, "worker is not in credential tenant")
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

func (s *Server) requestSessionPreview(ctx context.Context, workerID, name, sessionID string, lines int) (protocol.SessionPreview, error) {
	requestID := "preview_" + randomID()
	reply := make(chan protocol.Envelope, 1)
	s.mu.Lock()
	s.previews[requestID] = reply
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.previews, requestID)
		s.mu.Unlock()
	}()

	if err := s.sendToWorkerWithID(workerID, protocol.TypeSessionPreview, protocol.SessionPreviewRequest{Lines: lines}, name, sessionID, requestID); err != nil {
		return protocol.SessionPreview{}, err
	}
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return protocol.SessionPreview{}, ctx.Err()
	case <-timer.C:
		return protocol.SessionPreview{}, fmt.Errorf("session preview timed out")
	case env := <-reply:
		if env.Type == protocol.TypeError {
			var payload protocol.ErrorPayload
			_ = env.DecodePayload(&payload)
			return protocol.SessionPreview{}, errors.New(payload.Message)
		}
		var preview protocol.SessionPreview
		if err := env.DecodePayload(&preview); err != nil {
			return protocol.SessionPreview{}, err
		}
		return preview, nil
	}
}

func (s *Server) completePreview(env protocol.Envelope) bool {
	if env.ID == "" {
		return false
	}
	s.mu.RLock()
	reply := s.previews[env.ID]
	s.mu.RUnlock()
	if reply == nil {
		return false
	}
	select {
	case reply <- env:
	default:
	}
	return true
}

func (s *Server) sendToWorker(workerID, messageType string, payload any, name, sessionID string, streamID ...string) error {
	return s.sendToWorkerWithID(workerID, messageType, payload, name, sessionID, "", streamID...)
}

func (s *Server) sendToWorkerWithID(workerID, messageType string, payload any, name, sessionID, requestID string, streamID ...string) error {
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
	env := protocol.Envelope{Type: messageType, ID: requestID, WorkerID: workerID, SessionID: sessionID, Payload: raw}
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

func (s *Server) workerInTenant(workerID, tenantID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	worker := s.workers[workerID]
	return worker != nil && worker.tenantID == tenantID
}

func (s *Server) workerTenantIDLocked(workerID string) string {
	worker := s.workers[workerID]
	if worker == nil {
		return ""
	}
	return worker.tenantID
}

func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth, ok := s.authenticateRole(r, "")
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), authContextKey{}, auth)))
	}
}

func (s *Server) requireRole(role string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth, ok := s.authenticateRole(r, role)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), authContextKey{}, auth)))
	}
}

func (s *Server) authorized(r *http.Request) bool {
	return s.authorizedRole(r, "")
}

func (s *Server) authorizedRole(r *http.Request, role string) bool {
	_, ok := s.authenticateRole(r, role)
	return ok
}

func (s *Server) authenticateRole(r *http.Request, role string) (authContext, bool) {
	token := bearerOrQueryToken(r)
	if s.token != "" && token == s.token {
		return authContext{Admin: true}, true
	}
	credential, ok := s.auth.Credential(token)
	if !ok {
		return authContext{}, false
	}
	if role != "" && credential.Role != role {
		return authContext{}, false
	}
	return authContext{Credential: credential}, true
}

func requestAuth(r *http.Request) authContext {
	auth, _ := r.Context().Value(authContextKey{}).(authContext)
	return auth
}

func authTenantID(auth authContext) string {
	if auth.Admin {
		return "admin"
	}
	return auth.Credential.TenantID
}

func safeQueryInt(value string, fallback int) int {
	if value == "" {
		return fallback
	}
	number, err := strconv.Atoi(value)
	if err != nil || number <= 0 {
		return fallback
	}
	return number
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

func (s *Server) requestBaseURL(r *http.Request) string {
	if s.publicURL != "" {
		return s.publicURL
	}
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

const installScriptTemplate = `#!/bin/sh
set -eu

ROLE="${1:-worker}"
shift || true

REPO="${AGENTMUX_REPO:-%s}"
VERSION="${AGENTMUX_VERSION:-latest}"
HUB_HTTP="%s"
HUB_WS="%s"
BIN_DIR="${AGENTMUX_BIN_DIR:-$HOME/.local/bin}"
BIN="$BIN_DIR/agentmux"

mkdir -p "$BIN_DIR"

if command -v agentmux >/dev/null 2>&1; then
  BIN="$(command -v agentmux)"
elif command -v go >/dev/null 2>&1 && [ -f "./cmd/agentmux/main.go" ]; then
  go build -o "$BIN" ./cmd/agentmux
elif command -v curl >/dev/null 2>&1 && command -v tar >/dev/null 2>&1; then
  os="$(uname -s | tr '[:upper:]' '[:lower:]')"
  arch="$(uname -m)"
  case "$arch" in
    x86_64|amd64) arch="amd64" ;;
    aarch64|arm64) arch="arm64" ;;
    *) echo "unsupported architecture: $arch" >&2; exit 1 ;;
  esac
  case "$os" in
    linux|darwin) ;;
    *) echo "unsupported OS: $os" >&2; exit 1 ;;
  esac
  asset="agentmux-${os}-${arch}.tar.gz"
  if [ "$VERSION" = "latest" ]; then
    url="https://github.com/${REPO}/releases/latest/download/${asset}"
  else
    url="https://github.com/${REPO}/releases/download/${VERSION}/${asset}"
  fi
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT
  echo "Downloading $url" >&2
  curl -fsSL "$url" -o "$tmp/$asset"
  tar -xzf "$tmp/$asset" -C "$tmp"
  install -m 0755 "$tmp/agentmux-${os}-${arch}" "$BIN"
else
  echo "agentmux binary is not installed." >&2
  echo "Install curl+tar, put agentmux in PATH, or run this script from a source checkout with Go installed." >&2
  exit 1
fi

case "$ROLE" in
  worker)
    exec "$BIN" worker --hub "$HUB_WS" "$@"
    ;;
  control)
    exec "$BIN" control app --hub "$HUB_HTTP" "$@"
    ;;
  *)
    echo "usage: install.sh worker|control [agentmux flags]" >&2
    exit 2
    ;;
esac
`

func controlPageURL(baseURL string, token string) string {
	u, err := url.Parse(baseURL + "/control")
	if err != nil {
		return baseURL + "/control"
	}
	q := u.Query()
	q.Set("signal", token)
	u.RawQuery = q.Encode()
	return u.String()
}

func serveEmbeddedControl(w http.ResponseWriter, r *http.Request) bool {
	dist, err := fs.Sub(webDist, "webdist")
	if err != nil {
		return false
	}
	index, err := fs.ReadFile(dist, "index.html")
	if err != nil {
		return false
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(index)
	return true
}

var landingTemplate = template.Must(template.New("landing").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>AgentMux</title>
  <style>
    :root {
      color-scheme: dark;
      --bg: #070909;
      --panel: #101414;
      --panel-2: #171d1d;
      --line: #273131;
      --text: #edf5f2;
      --muted: #9fb0aa;
      --accent: #35c98f;
      --accent-2: #76a9ff;
      --warn: #f2b84b;
      font-family: Inter, ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
    }
    * { box-sizing: border-box; }
    html { min-height: 100%; background: var(--bg); }
    body { margin: 0; background: radial-gradient(circle at 70% 10%, rgba(53, 201, 143, .14), transparent 32rem), var(--bg); color: var(--text); }
    a { color: inherit; }
    code, pre { font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; }
    .shell { min-height: 100vh; display: flex; flex-direction: column; }
    .nav { height: 56px; display: flex; align-items: center; justify-content: space-between; padding: 0 28px; border-bottom: 1px solid var(--line); background: rgba(7, 9, 9, .78); backdrop-filter: blur(18px); position: sticky; top: 0; z-index: 10; }
    .brand { display: flex; align-items: center; gap: 10px; font-weight: 750; letter-spacing: 0; }
    .mark { width: 24px; height: 24px; border-radius: 6px; background: linear-gradient(135deg, var(--accent), var(--accent-2)); box-shadow: 0 0 22px rgba(53, 201, 143, .35); }
    .navlinks { display: flex; align-items: center; gap: 16px; color: var(--muted); font-size: 14px; }
    .navlinks a { text-decoration: none; }
    main { width: min(1180px, calc(100% - 40px)); margin: 0 auto; }
    .hero { display: grid; grid-template-columns: minmax(0, 1fr) 500px; gap: 36px; align-items: center; padding: 56px 0 34px; }
    h1 { margin: 0; font-size: clamp(42px, 7vw, 76px); line-height: .96; letter-spacing: 0; max-width: 760px; }
    .lead { margin: 22px 0 0; max-width: 660px; color: var(--muted); font-size: 18px; line-height: 1.6; }
    .actions { display: flex; flex-wrap: wrap; align-items: center; gap: 12px; margin-top: 28px; }
    button, .button { min-height: 38px; display: inline-flex; align-items: center; justify-content: center; gap: 8px; border: 1px solid var(--line); border-radius: 7px; background: var(--panel-2); color: var(--text); padding: 0 14px; font: inherit; text-decoration: none; cursor: pointer; }
    button.primary, .button.primary { background: var(--accent); border-color: var(--accent); color: #06130e; font-weight: 750; }
    .status { color: var(--muted); font-size: 13px; }
    .terminal { border: 1px solid var(--line); border-radius: 8px; background: #050606; overflow: hidden; box-shadow: 0 24px 80px rgba(0, 0, 0, .35); }
    .terminal-head { height: 34px; display: flex; align-items: center; justify-content: space-between; border-bottom: 1px solid var(--line); padding: 0 12px; color: var(--muted); font-size: 12px; background: #0d1111; }
    .terminal-body { padding: 16px; min-height: 320px; display: grid; align-content: start; gap: 12px; }
    .line { color: #c9d8d2; font-size: 13px; line-height: 1.55; }
    .line b { color: var(--accent); font-weight: 650; }
    .cards { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 14px; padding: 24px 0 50px; }
    .card { border: 1px solid var(--line); border-radius: 8px; background: rgba(16, 20, 20, .82); padding: 18px; }
    .card h2 { margin: 0 0 8px; font-size: 16px; }
    .card p { margin: 0; color: var(--muted); line-height: 1.55; font-size: 14px; }
    .panel { border: 1px solid var(--line); border-radius: 8px; background: rgba(16, 20, 20, .9); padding: 18px; margin-bottom: 50px; }
    .panel-head { display: flex; align-items: center; justify-content: space-between; gap: 18px; margin-bottom: 14px; }
    .panel h2 { margin: 0; font-size: 22px; }
    .panel p { margin: 4px 0 0; color: var(--muted); }
    .grid { display: grid; grid-template-columns: 1fr 1fr; gap: 14px; }
    .command { border: 1px solid var(--line); border-radius: 7px; background: #060808; overflow: hidden; }
    .command-title { display: flex; align-items: center; justify-content: space-between; gap: 12px; border-bottom: 1px solid var(--line); color: var(--muted); font-size: 12px; padding: 8px 10px; }
    pre { margin: 0; padding: 12px; overflow-x: auto; white-space: pre-wrap; overflow-wrap: anywhere; color: #d7e5df; font-size: 13px; line-height: 1.55; }
    .pill { display: inline-flex; align-items: center; gap: 7px; border: 1px solid var(--line); border-radius: 999px; padding: 6px 10px; color: var(--muted); font-size: 13px; }
    .dot { width: 7px; height: 7px; border-radius: 999px; background: var(--accent); }
    .footer { border-top: 1px solid var(--line); color: var(--muted); font-size: 13px; padding: 18px 0 34px; display: flex; justify-content: space-between; gap: 16px; }
    @media (max-width: 900px) {
      .nav { padding: 0 18px; }
      .navlinks { display: none; }
      main { width: min(100% - 28px, 720px); }
      .hero, .grid, .cards { grid-template-columns: 1fr; }
      .hero { padding-top: 34px; }
    }
  </style>
</head>
<body>
  <div class="shell">
    <nav class="nav">
      <div class="brand"><span class="mark"></span><span>AgentMux</span></div>
      <div class="navlinks">
        <a href="/control">Web Control</a>
        <a href="/install.sh">install.sh</a>
        <a href="#quickstart">Quick Start</a>
      </div>
    </nav>
    <main>
      <section class="hero">
        <div>
          <div class="pill"><span class="dot"></span>tmux-first remote control plane for coding agents</div>
          <h1>Bring every agent session back to one hub.</h1>
          <p class="lead">AgentMux keeps Codex, Claude, Gemini, OpenCode, and plain shells unaware of remote access. Workers own local tmux sessions, Hub routes identity and WebSockets, Control gives you a browser and CLI surface from anywhere.</p>
          <div class="actions">
            <button id="mint" class="primary">Generate join signal</button>
            <a class="button" href="/control">Open Web Control</a>
            <span id="status" class="status">Hub {{.BaseURL}} · Worker {{.WSURL}}</span>
          </div>
        </div>
        <div class="terminal" aria-label="AgentMux flow">
          <div class="terminal-head"><span>agentmux flow</span><span>wss relay</span></div>
          <div class="terminal-body">
            <div class="line"><b>1.</b> Generate a short-lived signal from this page.</div>
            <div class="line"><b>2.</b> Run one command on a worker machine.</div>
            <div class="line"><b>3.</b> Open Web Control and split sessions into panes.</div>
            <div class="line"><b>4.</b> The agent only sees a local tmux terminal.</div>
          </div>
        </div>
      </section>

      <section class="cards">
        <div class="card"><h2>Agent-unaware</h2><p>No agent SDK, callback server, or vendor-specific remote feature. AgentMux attaches below the agent at the shell/tmux layer.</p></div>
        <div class="card"><h2>Cloudflare-ready</h2><p>Run Hub behind Cloudflare Tunnel or a proxy. HTTPS becomes WSS, and workers keep outbound-only connectivity by default.</p></div>
        <div class="card"><h2>Multi-session control</h2><p>The Web Control surface supports resizable panes, drag placement, session creation, and browser credentials.</p></div>
      </section>

      <section id="quickstart" class="panel">
        <div class="panel-head">
          <div>
            <h2>Quick Start</h2>
            <p>Generate a signal, then use the commands below before it expires.</p>
          </div>
          <button id="mint2">Generate</button>
        </div>
        <div id="result" class="grid">
          <div class="command">
            <div class="command-title"><span>Local hub</span></div>
            <pre>agentmux hub --addr 0.0.0.0:8080 --data ./agentmux.db --public-url {{.BaseURL}}</pre>
          </div>
          <div class="command">
            <div class="command-title"><span>Cloudflare tunnel</span></div>
            <pre>cloudflared tunnel --url http://127.0.0.1:8080</pre>
          </div>
        </div>
      </section>

      <footer class="footer">
        <span>Default admin token stays local. Signals exchange into scoped credentials.</span>
        <span>Docs: README.md · docs/API.md · docs/PRODUCT_ARCHITECTURE.md</span>
      </footer>
    </main>
  </div>
  <script>
    const result = document.getElementById('result');
    const status = document.getElementById('status');
    document.getElementById('mint').addEventListener('click', mint);
    document.getElementById('mint2').addEventListener('click', mint);
    async function mint() {
      status.textContent = 'Generating signal...';
      const res = await fetch('/api/signals', { method: 'POST' });
      if (!res.ok) {
        status.textContent = 'Failed: ' + await res.text();
        return;
      }
      const data = await res.json();
      status.textContent = 'Signal ready · expires ' + new Date(data.expires_at).toLocaleString();
      result.innerHTML =
        commandBlock('Signal', data.signal || data.token, false) +
        commandBlock('Worker source/dev', data.worker_command, true) +
        commandBlock('Worker one-line script', 'curl -fsSL ' + location.origin + '/install.sh | sh -s -- worker --join ' + shellQuote(data.signal || data.token) + ' --name "$(hostname)"', true) +
        commandBlock('Web Control', data.control_url, false) +
        commandBlock('Control CLI', data.control_command, true) +
        commandBlock('Control app script', 'curl -fsSL ' + location.origin + '/install.sh | sh -s -- control --join ' + shellQuote(data.signal || data.token), true);
    }
    function commandBlock(title, value, copyable) {
      return '<div class="command"><div class="command-title"><span>' + escapeHTML(title) + '</span>' +
        (copyable ? '<button data-copy="' + escapeAttr(value) + '" onclick="copyValue(this)">Copy</button>' : '') +
        '</div><pre>' + escapeHTML(value) + '</pre></div>';
    }
    async function copyValue(button) {
      await navigator.clipboard.writeText(button.getAttribute('data-copy') || '');
      button.textContent = 'Copied';
      setTimeout(() => button.textContent = 'Copy', 1000);
    }
    function shellQuote(value) {
      return "'" + String(value).replace(/'/g, "'\\''") + "'";
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
    #terminal { width: 100%; height: 100%; min-width: 0; min-height: 0; }
    #terminal .xterm { width: 100%; height: 100%; }
    #terminal .xterm-viewport { width: 100% !important; }
    #terminal .xterm-screen { width: 100%; height: 100%; }
    #terminal .xterm-helper-textarea { left: 0; top: 0; }
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
        <label for="token">Credential</label>
        <input id="token" autocomplete="off" spellcheck="false" placeholder="amx_cred_... or dev token">
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
    const terminalWrap = terminalEl.parentElement;
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
    let resizeObserver = null;
    let lastSize = { cols: 0, rows: 0 };

    const query = new URLSearchParams(location.search);
    const debug = query.get('debug') === '1';
    const initialSignal = query.get('signal') || '';
    const initialToken = query.get('token') || localStorage.getItem('agentmux.token') || '';
    tokenInput.value = initialToken;
    if (initialSignal) {
      exchangeSignal(initialSignal);
    } else if (initialToken) {
      refreshAll();
    }

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

    async function exchangeSignal(signal) {
      setStatus('Exchanging signal', 'Requesting a scoped browser credential.', 'warn');
      const res = await fetch(hubBase + '/api/exchange', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          signal,
          role: 'control',
          device_name: navigator.userAgent,
          device_id: localStorage.getItem('agentmux.control_device_id') || ''
        })
      });
      if (!res.ok) {
        setStatus('Signal exchange failed', await res.text(), 'err');
        return;
      }
      const data = await res.json();
      tokenInput.value = data.credential;
      localStorage.setItem('agentmux.token', data.credential);
      localStorage.setItem('agentmux.control_device_id', data.device_id);
      setStatus('Credential ready', data.credential_id + ' · ' + data.tenant_id, 'ok');
      await refreshAll();
    }

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
      syncTerminalElementSize();
      term.open(terminalEl);
      term.focus();
      watchTerminalSize();
      fit.fit();
      streamId = makeStreamID(sessionID);
      const wsURL = wsBase + '/ws/control?token=' + encodeURIComponent(tokenInput.value.trim());
      socket = new WebSocket(wsURL);
      setStatus('Connecting ' + sessionID, 'Opening remote PTY stream.', 'warn');
      detachButton.disabled = false;

      socket.addEventListener('open', () => {
        refitTerminal(false);
        lastSize = { cols: term.cols, rows: term.rows };
        debugLayout('control.open');
        sendEnvelope('control.open', sessionID, { cols: term.cols, rows: term.rows });
        stabilizeInitialTerminalFit();
        setStatus('Attached ' + sessionID, 'stream ' + streamId, 'ok');
      });
      socket.addEventListener('message', event => handleMessage(event.data));
      socket.addEventListener('close', () => {
        detachButton.disabled = true;
        if (activeSession === sessionID) setStatus('Detached ' + sessionID, 'The browser control stream is closed.', 'warn');
      });
      socket.addEventListener('error', () => setStatus('WebSocket error', sessionID, 'err'));

      term.onData(data => sendEnvelope('control.input', sessionID, { data }));
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
        refitTerminal(false);
      }, 80);
    }

    function refitTerminal(force) {
      if (!term || !fit) return;
      syncTerminalElementSize();
      fit.fit();
      if (!term.cols || !term.rows) return;
      const changed = term.cols !== lastSize.cols || term.rows !== lastSize.rows;
      if (force || changed) {
        lastSize = { cols: term.cols, rows: term.rows };
        debugLayout('terminal.resize');
        sendEnvelope('terminal.resize', activeSession, { cols: term.cols, rows: term.rows });
      }
    }

    function stabilizeInitialTerminalFit() {
      requestAnimationFrame(() => refitTerminal(false));
      requestAnimationFrame(() => requestAnimationFrame(() => refitTerminal(false)));
      setTimeout(() => refitTerminal(false), 80);
      setTimeout(() => refitTerminal(false), 240);
      setTimeout(() => refitTerminal(false), 600);
    }

    function watchTerminalSize() {
      stopWatchingTerminalSize();
      if (!('ResizeObserver' in window)) {
        window.addEventListener('resize', scheduleResize);
        return;
      }
      resizeObserver = new ResizeObserver(() => scheduleResize());
      resizeObserver.observe(terminalWrap);
    }

    function stopWatchingTerminalSize() {
      window.removeEventListener('resize', scheduleResize);
      if (resizeObserver) {
        resizeObserver.disconnect();
        resizeObserver = null;
      }
    }

    function syncTerminalElementSize() {
      if (!terminalWrap) return;
      const rect = terminalWrap.getBoundingClientRect();
      const style = getComputedStyle(terminalWrap);
      const horizontalPadding = parseFloat(style.paddingLeft) + parseFloat(style.paddingRight);
      const verticalPadding = parseFloat(style.paddingTop) + parseFloat(style.paddingBottom);
      const width = Math.max(0, Math.floor(rect.width - horizontalPadding));
      const height = Math.max(0, Math.floor(rect.height - verticalPadding));
      if (!width || !height) return;
      terminalEl.style.width = width + 'px';
      terminalEl.style.height = height + 'px';
    }

    function debugLayout(label) {
      if (!debug || !term) return;
      const wrap = rectInfo(terminalWrap);
      const terminal = rectInfo(terminalEl);
      const xterm = rectInfo(terminalEl.querySelector('.xterm'));
      const screen = rectInfo(terminalEl.querySelector('.xterm-screen'));
      console.debug('[agentmux]', label, {
        wrap,
        terminal,
        xterm,
        screen,
        cols: term.cols,
        rows: term.rows,
        lastSize,
        activeSession
      });
    }

    function rectInfo(element) {
      if (!element) return null;
      const rect = element.getBoundingClientRect();
      return {
        width: Math.round(rect.width),
        height: Math.round(rect.height),
        clientWidth: element.clientWidth,
        clientHeight: element.clientHeight,
        offsetWidth: element.offsetWidth,
        offsetHeight: element.offsetHeight
      };
    }

    function detach() {
      const previousSession = activeSession;
      stopWatchingTerminalSize();
      clearTimeout(resizeTimer);
      lastSize = { cols: 0, rows: 0 };
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
