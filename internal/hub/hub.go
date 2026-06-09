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

//go:embed webdist docassets
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
	mux.HandleFunc("/agentmux-mark.svg", s.handleRootAsset)
	mux.HandleFunc("/install.sh", s.handleInstallScript)
	mux.HandleFunc("/control", s.handleControlPage)
	mux.HandleFunc("/assets/", s.handleWebAssets)
	mux.HandleFunc("/docassets/", s.handleDocAssets)
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
		BaseURL:          baseURL,
		WSURL:            websocketBase(baseURL),
		ReleaseRepo:      s.releaseRepo,
		GitHubURL:        "https://github.com/" + s.releaseRepo,
		ReleasesURL:      "https://github.com/" + s.releaseRepo + "/releases",
		LatestReleaseAPI: "https://api.github.com/repos/" + s.releaseRepo + "/releases/latest",
		ContainerImage:   "ghcr.io/" + strings.ToLower(s.releaseRepo),
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

func (s *Server) handleDocAssets(w http.ResponseWriter, r *http.Request) {
	assets, err := fs.Sub(webDist, "docassets")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	http.StripPrefix("/docassets/", http.FileServer(http.FS(assets))).ServeHTTP(w, r)
}

func (s *Server) handleRootAsset(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/agentmux-mark.svg" {
		http.NotFound(w, r)
		return
	}
	data, err := webDist.ReadFile("webdist/agentmux-mark.svg")
	if err != nil {
		data, err = webDist.ReadFile("docassets/agentmux-mark.svg")
	}
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	_, _ = w.Write(data)
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
	BaseURL          string
	WSURL            string
	ReleaseRepo      string
	GitHubURL        string
	ReleasesURL      string
	LatestReleaseAPI string
	ContainerImage   string
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
      --bg: #050707;
      --panel: rgba(13, 18, 18, .78);
      --panel-2: rgba(22, 29, 29, .88);
      --line: rgba(132, 154, 146, .22);
      --text: #edf7f3;
      --muted: #9fb0aa;
      --accent: #36d693;
      --accent-2: #79a9ff;
      --accent-3: #b68cff;
      --warn: #f2c14e;
      font-family: Inter, ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
    }
    * { box-sizing: border-box; }
    html { min-height: 100%; background: var(--bg); scroll-behavior: smooth; }
    body {
      margin: 0;
      color: var(--text);
      background:
        linear-gradient(120deg, rgba(54, 214, 147, .12), transparent 34%),
        radial-gradient(circle at 78% 12%, rgba(121, 169, 255, .18), transparent 28rem),
        radial-gradient(circle at 22% 42%, rgba(182, 140, 255, .12), transparent 30rem),
        var(--bg);
      overflow-x: hidden;
    }
    body::before {
      content: "";
      position: fixed;
      inset: -20%;
      pointer-events: none;
      background:
        radial-gradient(circle at 25% 22%, rgba(54, 214, 147, .16), transparent 26rem),
        radial-gradient(circle at 76% 28%, rgba(121, 169, 255, .14), transparent 24rem),
        linear-gradient(115deg, transparent 20%, rgba(255, 255, 255, .045), transparent 44%);
      filter: blur(20px);
      opacity: .82;
      animation: auroraShift 16s ease-in-out infinite alternate;
    }
    body::after {
      content: "";
      position: fixed;
      inset: 0;
      pointer-events: none;
      background-image:
        linear-gradient(rgba(255,255,255,.035) 1px, transparent 1px),
        linear-gradient(90deg, rgba(255,255,255,.028) 1px, transparent 1px);
      background-size: 48px 48px;
      mask-image: linear-gradient(to bottom, rgba(0,0,0,.8), transparent 72%);
    }
    a { color: inherit; }
    code, pre { font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; }
    button, .button {
      min-height: 38px;
      display: inline-flex;
      align-items: center;
      justify-content: center;
      gap: 8px;
      border: 1px solid var(--line);
      border-radius: 7px;
      background: rgba(18, 24, 24, .86);
      color: var(--text);
      padding: 0 14px;
      font: inherit;
      text-decoration: none;
      cursor: pointer;
      transition: transform .18s ease, border-color .18s ease, background .18s ease;
    }
    button:hover, .button:hover { transform: translateY(-1px); border-color: rgba(54, 214, 147, .45); }
    button.primary, .button.primary { background: linear-gradient(135deg, var(--accent), #8df3c5); border-color: transparent; color: #06130e; font-weight: 760; }
    .shell { position: relative; z-index: 1; min-height: 100vh; display: flex; flex-direction: column; }
    .nav {
      height: 66px;
      display: flex;
      align-items: center;
      justify-content: space-between;
      padding: 0 28px;
      border-bottom: 1px solid var(--line);
      background: rgba(5, 7, 7, .72);
      backdrop-filter: blur(22px);
      position: sticky;
      top: 0;
      z-index: 20;
    }
    .brand { display: flex; align-items: center; gap: 11px; font-weight: 780; letter-spacing: 0; }
    .mark { width: 38px; height: 38px; border-radius: 10px; box-shadow: 0 0 28px rgba(54, 214, 147, .34); }
    .navlinks { display: flex; align-items: center; gap: 14px; color: var(--muted); font-size: 14px; }
    .navlinks a { text-decoration: none; }
    .github-icon { width: 16px; height: 16px; flex: 0 0 auto; }
    .github-link, .with-icon { display: inline-flex; align-items: center; gap: 7px; }
    .lang { display: flex; align-items: center; gap: 4px; border: 1px solid var(--line); border-radius: 999px; padding: 3px; background: rgba(7, 10, 10, .72); }
    .lang button { min-height: 26px; border: 0; border-radius: 999px; padding: 0 9px; color: var(--muted); background: transparent; font-size: 12px; }
    .lang button.active { background: rgba(54, 214, 147, .18); color: var(--text); }
    main { width: min(1360px, calc(100% - 44px)); margin: 0 auto; }
    .hero { display: grid; grid-template-columns: minmax(0, .9fr) minmax(540px, 1.15fr); gap: 34px; align-items: center; padding: 62px 0 36px; }
    h1 { margin: 0; font-size: clamp(43px, 6.4vw, 84px); line-height: .95; letter-spacing: 0; max-width: 780px; }
    .lead { margin: 22px 0 0; max-width: 710px; color: var(--muted); font-size: 18px; line-height: 1.62; }
    .actions { display: flex; flex-wrap: wrap; align-items: center; gap: 12px; margin-top: 28px; }
    .status { width: 100%; color: var(--muted); font-size: 13px; }
    .pill { display: inline-flex; align-items: center; gap: 8px; border: 1px solid var(--line); border-radius: 999px; padding: 7px 11px; color: #cfe0da; font-size: 13px; background: rgba(10, 14, 14, .66); margin-bottom: 18px; }
    .dot { width: 8px; height: 8px; border-radius: 999px; background: var(--accent); box-shadow: 0 0 18px rgba(54, 214, 147, .85); }
    .hero-visual {
      border: 1px solid var(--line);
      border-radius: 8px;
      background: rgba(5, 7, 7, .84);
      overflow: hidden;
      box-shadow: 0 28px 90px rgba(0, 0, 0, .42), inset 0 1px 0 rgba(255, 255, 255, .04);
      transform: perspective(1400px) rotateY(-3deg) rotateX(2deg);
    }
    .visual-button { width: 100%; display: block; min-height: 0; padding: 0; border: 0; border-radius: 0; background: transparent; text-align: left; transform: none; }
    .visual-button:hover { transform: none; }
    .hero-visual img, .visual img { display: block; width: 100%; height: auto; }
    .hero-visual .caption { display: flex; align-items: center; justify-content: space-between; gap: 12px; border-top: 1px solid var(--line); padding: 10px 12px; color: var(--muted); font-size: 12px; background: rgba(6, 8, 8, .7); }
    .cards { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 14px; padding: 22px 0 52px; }
    .card, .panel, .visual {
      border: 1px solid var(--line);
      border-radius: 8px;
      background: var(--panel);
      box-shadow: inset 0 1px 0 rgba(255, 255, 255, .035);
      backdrop-filter: blur(18px);
    }
    .card { padding: 18px; }
    .card h2 { margin: 0 0 8px; font-size: 16px; }
    .card p { margin: 0; color: var(--muted); line-height: 1.55; font-size: 14px; }
    .section-head { display: flex; align-items: end; justify-content: space-between; gap: 18px; margin-bottom: 14px; }
    .section-head h2, .panel h2 { margin: 0; font-size: 24px; }
    .section-head p, .panel p { margin: 5px 0 0; color: var(--muted); line-height: 1.5; }
    .visuals { display: grid; gap: 18px; padding: 0 0 56px; }
    .visual { display: grid; grid-template-columns: minmax(0, 1.36fr) minmax(280px, .64fr); overflow: hidden; transition: transform .18s ease, border-color .18s ease; }
    .visual:hover { transform: translateY(-2px); border-color: rgba(54, 214, 147, .42); }
    .visual:nth-child(even) { grid-template-columns: minmax(280px, .64fr) minmax(0, 1.36fr); }
    .visual:nth-child(even) .visual-copy { order: -1; border-left: 0; border-right: 1px solid var(--line); }
    .visual .visual-button { background: #050707; }
    .visual img { min-height: 280px; object-fit: cover; }
    .visual-copy { padding: 20px; border-left: 1px solid var(--line); display: flex; flex-direction: column; justify-content: center; }
    .visual h2 { margin: 0 0 8px; font-size: 19px; }
    .visual p { margin: 0; color: var(--muted); line-height: 1.55; font-size: 14px; }
    .visual .hint { margin-top: 14px; color: #cce7dc; font-size: 12px; }
    .panel { padding: 18px; margin-bottom: 52px; }
    .release-panel { display: grid; grid-template-columns: minmax(0, .8fr) minmax(0, 1.2fr); gap: 18px; align-items: stretch; margin-bottom: 56px; }
    .release-meta { display: grid; align-content: start; gap: 12px; }
    .release-version { width: fit-content; border: 1px solid rgba(54, 214, 147, .38); border-radius: 999px; padding: 7px 11px; color: #d7f8ea; background: rgba(54, 214, 147, .12); font-size: 13px; font-weight: 680; }
    .release-note { min-height: 150px; max-height: 270px; overflow: auto; white-space: pre-wrap; color: #c7d8d2; line-height: 1.55; font-size: 13px; border: 1px solid var(--line); border-radius: 7px; background: rgba(3, 5, 5, .72); padding: 12px; }
    .release-actions { display: flex; flex-wrap: wrap; gap: 10px; }
    .asset-list { display: flex; flex-wrap: wrap; gap: 7px; margin-top: 10px; }
    .asset-list a { border: 1px solid var(--line); border-radius: 999px; padding: 5px 8px; color: var(--muted); text-decoration: none; font-size: 12px; background: rgba(10, 14, 14, .72); }
    .panel-head { display: flex; align-items: center; justify-content: space-between; gap: 18px; margin-bottom: 14px; }
    .grid { display: grid; grid-template-columns: 1fr 1fr; gap: 14px; }
    .command { border: 1px solid var(--line); border-radius: 7px; background: rgba(3, 5, 5, .82); overflow: hidden; }
    .command-title { display: flex; align-items: center; justify-content: space-between; gap: 12px; border-bottom: 1px solid var(--line); color: var(--muted); font-size: 12px; padding: 8px 10px; }
    pre { margin: 0; padding: 12px; overflow-x: auto; white-space: pre-wrap; overflow-wrap: anywhere; color: #d7e5df; font-size: 13px; line-height: 1.55; }
    .footer { border-top: 1px solid var(--line); color: var(--muted); font-size: 13px; padding: 18px 0 34px; display: flex; justify-content: space-between; gap: 16px; }
    .lightbox { position: fixed; inset: 0; z-index: 50; display: none; align-items: center; justify-content: center; padding: 26px; background: rgba(0, 0, 0, .76); backdrop-filter: blur(16px); }
    .lightbox.open { display: flex; }
    .lightbox-panel { width: min(1340px, 100%); max-height: min(88vh, 900px); border: 1px solid var(--line); border-radius: 8px; background: #050707; overflow: hidden; box-shadow: 0 28px 120px rgba(0, 0, 0, .58); }
    .lightbox-head { height: 44px; display: flex; align-items: center; justify-content: space-between; gap: 12px; border-bottom: 1px solid var(--line); padding: 0 12px; color: var(--muted); }
    .lightbox img { display: block; width: 100%; max-height: calc(88vh - 44px); object-fit: contain; background: #050707; }
    @keyframes auroraShift {
      from { transform: translate3d(-2%, -1%, 0) scale(1); }
      to { transform: translate3d(2%, 1.5%, 0) scale(1.04); }
    }
    @media (prefers-reduced-motion: reduce) {
      *, body::before { animation: none !important; transition: none !important; }
      html { scroll-behavior: auto; }
    }
    @media (max-width: 1000px) {
      .nav { padding: 0 18px; }
      .navlinks a.hide-small { display: none; }
      main { width: min(100% - 28px, 760px); }
      .hero, .grid, .cards, .release-panel, .visual, .visual:nth-child(even) { grid-template-columns: 1fr; }
      .hero { padding-top: 36px; }
      .hero-visual { transform: none; }
      .visual-copy, .visual:nth-child(even) .visual-copy { order: 0; border-left: 0; border-right: 0; border-top: 1px solid var(--line); }
      .visual img { min-height: 0; object-fit: contain; }
      .section-head, .panel-head, .footer { align-items: flex-start; flex-direction: column; }
    }
    @media (max-width: 620px) {
      .navlinks { gap: 8px; }
      .navlinks a:not(.github-link) { display: none; }
      h1 { font-size: 42px; }
      .lead { font-size: 16px; }
      .lightbox { padding: 10px; }
    }
  </style>
</head>
<body>
  <div class="shell">
    <nav class="nav">
      <div class="brand"><img class="mark" src="/docassets/agentmux-mark.svg" alt=""><span>AgentMux</span></div>
      <div class="navlinks">
        <a href="/control" data-i18n="navControl">Web Control</a>
        <a class="hide-small" href="/install.sh">install.sh</a>
        <a class="hide-small" href="#quickstart" data-i18n="navQuick">Quick Start</a>
        <a class="github-link" href="{{.GitHubURL}}" rel="noreferrer">
          <svg class="github-icon" viewBox="0 0 24 24" aria-hidden="true"><path fill="currentColor" d="M12 .5a12 12 0 0 0-3.79 23.39c.6.11.82-.26.82-.58v-2.04c-3.34.73-4.04-1.42-4.04-1.42-.55-1.39-1.34-1.76-1.34-1.76-1.09-.75.08-.73.08-.73 1.21.08 1.85 1.24 1.85 1.24 1.07 1.84 2.82 1.31 3.51 1 .11-.78.42-1.31.76-1.61-2.66-.3-5.47-1.33-5.47-5.93 0-1.31.47-2.38 1.24-3.22-.12-.3-.54-1.52.12-3.18 0 0 1.01-.32 3.3 1.23a11.5 11.5 0 0 1 6.01 0c2.29-1.55 3.3-1.23 3.3-1.23.66 1.66.24 2.88.12 3.18.77.84 1.24 1.91 1.24 3.22 0 4.61-2.81 5.62-5.49 5.92.43.37.81 1.1.81 2.22v3.29c0 .32.22.69.83.57A12 12 0 0 0 12 .5Z"/></svg>
          <span data-i18n="navGithub">GitHub</span>
        </a>
        <div class="lang" aria-label="Language">
          <button type="button" data-lang="en">EN</button>
          <button type="button" data-lang="zh">中</button>
        </div>
      </div>
    </nav>
    <main>
      <section class="hero">
        <div>
          <div class="pill"><span class="dot"></span><span data-i18n="eyebrow">Open-source tmux control plane for coding agents</span></div>
          <h1 data-i18n="heroTitle">Bring every agent session back to one hub.</h1>
          <p class="lead" data-i18n="heroLead">AgentMux keeps Codex, Claude, Gemini, OpenCode, and plain shells unaware of remote access. Workers own local tmux sessions, Hub routes identity and WebSockets, Control gives you a browser and CLI surface from anywhere.</p>
          <div class="actions">
            <button id="mint" class="primary" data-i18n="generateSignal">Generate join signal</button>
            <a class="button" href="/control" data-i18n="openControl">Open Web Control</a>
            <a class="button with-icon" href="{{.GitHubURL}}" rel="noreferrer">
              <svg class="github-icon" viewBox="0 0 24 24" aria-hidden="true"><path fill="currentColor" d="M12 .5a12 12 0 0 0-3.79 23.39c.6.11.82-.26.82-.58v-2.04c-3.34.73-4.04-1.42-4.04-1.42-.55-1.39-1.34-1.76-1.34-1.76-1.09-.75.08-.73.08-.73 1.21.08 1.85 1.24 1.85 1.24 1.07 1.84 2.82 1.31 3.51 1 .11-.78.42-1.31.76-1.61-2.66-.3-5.47-1.33-5.47-5.93 0-1.31.47-2.38 1.24-3.22-.12-.3-.54-1.52.12-3.18 0 0 1.01-.32 3.3 1.23a11.5 11.5 0 0 1 6.01 0c2.29-1.55 3.3-1.23 3.3-1.23.66 1.66.24 2.88.12 3.18.77.84 1.24 1.91 1.24 3.22 0 4.61-2.81 5.62-5.49 5.92.43.37.81 1.1.81 2.22v3.29c0 .32.22.69.83.57A12 12 0 0 0 12 .5Z"/></svg>
              <span data-i18n="openGithub">Open-source on GitHub</span>
            </a>
            <span id="status" class="status">Hub {{.BaseURL}} · Worker {{.WSURL}}</span>
          </div>
        </div>
        <div class="hero-visual" aria-label="AgentMux system architecture">
          <button class="visual-button" type="button" data-full="/docassets/system-architecture.png" data-title="System architecture">
            <img src="/docassets/system-architecture.png" alt="AgentMux system architecture diagram">
          </button>
          <div class="caption"><span data-i18n="heroVisual">Hub, Worker, Control and WSS relay architecture</span><span data-i18n="clickZoom">Click to inspect</span></div>
        </div>
      </section>

      <section class="cards">
        <div class="card"><h2 data-i18n="cardAgentTitle">Agent-unaware</h2><p data-i18n="cardAgentBody">No agent SDK, callback server, or vendor-specific remote feature. AgentMux attaches below the agent at the shell/tmux layer.</p></div>
        <div class="card"><h2 data-i18n="cardCloudTitle">Cloudflare-ready</h2><p data-i18n="cardCloudBody">Run Hub behind Cloudflare Tunnel or a proxy. HTTPS becomes WSS, and workers keep outbound-only connectivity by default.</p></div>
        <div class="card"><h2 data-i18n="cardControlTitle">Multi-session control</h2><p data-i18n="cardControlBody">The Web Control surface supports resizable panes, drag placement, session creation, and browser credentials.</p></div>
      </section>

      <section id="release" class="panel release-panel" aria-label="AgentMux release information">
        <div class="release-meta">
          <div class="pill"><span class="dot"></span><span data-i18n="releaseEyebrow">GitHub release</span></div>
          <h2 data-i18n="releaseTitle">Latest version</h2>
          <p data-i18n="releaseLead">The landing page reads the latest GitHub release and shows the version, notes, binary assets, and container image.</p>
          <div id="release-version" class="release-version">v0.0.1</div>
          <div class="release-actions">
            <a id="release-link" class="button with-icon" href="{{.ReleasesURL}}" rel="noreferrer">
              <svg class="github-icon" viewBox="0 0 24 24" aria-hidden="true"><path fill="currentColor" d="M12 .5a12 12 0 0 0-3.79 23.39c.6.11.82-.26.82-.58v-2.04c-3.34.73-4.04-1.42-4.04-1.42-.55-1.39-1.34-1.76-1.34-1.76-1.09-.75.08-.73.08-.73 1.21.08 1.85 1.24 1.85 1.24 1.07 1.84 2.82 1.31 3.51 1 .11-.78.42-1.31.76-1.61-2.66-.3-5.47-1.33-5.47-5.93 0-1.31.47-2.38 1.24-3.22-.12-.3-.54-1.52.12-3.18 0 0 1.01-.32 3.3 1.23a11.5 11.5 0 0 1 6.01 0c2.29-1.55 3.3-1.23 3.3-1.23.66 1.66.24 2.88.12 3.18.77.84 1.24 1.91 1.24 3.22 0 4.61-2.81 5.62-5.49 5.92.43.37.81 1.1.81 2.22v3.29c0 .32.22.69.83.57A12 12 0 0 0 12 .5Z"/></svg>
              <span data-i18n="releaseOpen">Open release</span>
            </a>
          </div>
        </div>
        <div>
          <div class="command">
            <div class="command-title"><span data-i18n="dockerImage">Docker image</span></div>
            <pre id="docker-command">docker run --rm -p 8080:8080 -v agentmux-data:/var/lib/agentmux {{.ContainerImage}}:latest hub --addr 0.0.0.0:8080 --data /var/lib/agentmux/agentmux.db --public-url {{.BaseURL}}</pre>
          </div>
          <div id="release-note" class="release-note" data-i18n="releaseLoading">Loading latest release note from GitHub...</div>
          <div id="release-assets" class="asset-list"></div>
        </div>
      </section>

      <section aria-label="AgentMux architecture visuals">
        <div class="section-head">
          <div>
            <h2 data-i18n="visualTitle">Architecture in practice</h2>
            <p data-i18n="visualLead">Large technical diagrams are embedded directly into the Hub landing page and can be opened for detail review.</p>
          </div>
        </div>
        <div class="visuals">
          <article class="visual">
            <button class="visual-button" type="button" data-full="/docassets/onboarding.png" data-title="Signal onboarding">
              <img src="/docassets/onboarding.png" alt="AgentMux signal onboarding flow">
            </button>
            <div class="visual-copy"><h2 data-i18n="onboardingTitle">Signal onboarding</h2><p data-i18n="onboardingBody">Generate a short-lived signal, exchange it for scoped credentials, then connect workers and controls.</p><span class="hint" data-i18n="clickZoom">Click to inspect</span></div>
          </article>
          <article class="visual">
            <button class="visual-button" type="button" data-full="/docassets/cloudflare-deployment.png" data-title="Cloudflare deployment">
              <img src="/docassets/cloudflare-deployment.png" alt="AgentMux Cloudflare deployment topology">
            </button>
            <div class="visual-copy"><h2 data-i18n="cloudflareTitle">Cloudflare-ready deployment</h2><p data-i18n="cloudflareBody">Keep Hub and SQLite on your server while Cloudflare terminates HTTPS and WSS through a tunnel.</p><span class="hint" data-i18n="clickZoom">Click to inspect</span></div>
          </article>
          <article class="visual">
            <button class="visual-button" type="button" data-full="/docassets/web-control-workspace.png" data-title="Web Control workspace">
              <img src="/docassets/web-control-workspace.png" alt="AgentMux Web Control multi-pane workspace">
            </button>
            <div class="visual-copy"><h2 data-i18n="workspaceTitle">Multi-pane Web Control</h2><p data-i18n="workspaceBody">Operate multiple long-lived tmux-backed agent sessions from a compact browser workspace.</p><span class="hint" data-i18n="clickZoom">Click to inspect</span></div>
          </article>
        </div>
      </section>

      <section id="quickstart" class="panel">
        <div class="panel-head">
          <div>
            <h2 data-i18n="quickTitle">Quick Start</h2>
            <p data-i18n="quickLead">Install the release binary through the generated script, then connect before the signal expires.</p>
          </div>
          <button id="mint2" data-i18n="generate">Generate</button>
        </div>
        <div id="result" class="grid">
          <div class="command">
            <div class="command-title"><span data-i18n="installBinary">Install binary</span></div>
            <pre>curl -fsSL {{.BaseURL}}/install.sh | sh -s -- control --join amx_sig_...</pre>
          </div>
          <div class="command">
            <div class="command-title"><span data-i18n="cloudflareTunnel">Cloudflare tunnel</span></div>
            <pre>cloudflared tunnel --url http://127.0.0.1:8080</pre>
          </div>
        </div>
      </section>

      <footer class="footer">
        <span data-i18n="footerSecurity">Default admin token stays local. Signals exchange into scoped credentials.</span>
        <span><span data-i18n="footerDocs">Docs</span>: README.md · docs/USAGE.md · docs/API.md · docs/PRODUCT_ARCHITECTURE.md · <a class="with-icon" href="{{.GitHubURL}}" rel="noreferrer"><svg class="github-icon" viewBox="0 0 24 24" aria-hidden="true"><path fill="currentColor" d="M12 .5a12 12 0 0 0-3.79 23.39c.6.11.82-.26.82-.58v-2.04c-3.34.73-4.04-1.42-4.04-1.42-.55-1.39-1.34-1.76-1.34-1.76-1.09-.75.08-.73.08-.73 1.21.08 1.85 1.24 1.85 1.24 1.07 1.84 2.82 1.31 3.51 1 .11-.78.42-1.31.76-1.61-2.66-.3-5.47-1.33-5.47-5.93 0-1.31.47-2.38 1.24-3.22-.12-.3-.54-1.52.12-3.18 0 0 1.01-.32 3.3 1.23a11.5 11.5 0 0 1 6.01 0c2.29-1.55 3.3-1.23 3.3-1.23.66 1.66.24 2.88.12 3.18.77.84 1.24 1.91 1.24 3.22 0 4.61-2.81 5.62-5.49 5.92.43.37.81 1.1.81 2.22v3.29c0 .32.22.69.83.57A12 12 0 0 0 12 .5Z"/></svg>{{.ReleaseRepo}}</a></span>
      </footer>
    </main>
  </div>
  <div id="lightbox" class="lightbox" role="dialog" aria-modal="true" aria-label="Image preview">
    <div class="lightbox-panel">
      <div class="lightbox-head"><span id="lightbox-title">Preview</span><button id="lightbox-close" type="button">Esc</button></div>
      <img id="lightbox-img" alt="">
    </div>
  </div>
  <script>
    const baseStatus = 'Hub {{.BaseURL}} · Worker {{.WSURL}}';
    const latestReleaseAPI = '{{.LatestReleaseAPI}}';
    const fallbackReleaseURL = '{{.ReleasesURL}}';
    const containerImage = '{{.ContainerImage}}';
    const result = document.getElementById('result');
    const status = document.getElementById('status');
    let latestRelease = null;
    const dictionaries = {
      en: {
        navControl: 'Web Control',
        navQuick: 'Quick Start',
        navGithub: 'GitHub',
        eyebrow: 'Open-source tmux control plane for coding agents',
        heroTitle: 'Bring every agent session back to one hub.',
        heroLead: 'AgentMux keeps Codex, Claude, Gemini, OpenCode, and plain shells unaware of remote access. Workers own local tmux sessions, Hub routes identity and WebSockets, Control gives you a browser and CLI surface from anywhere.',
        generateSignal: 'Generate join signal',
        openControl: 'Open Web Control',
        openGithub: 'Open-source on GitHub',
        heroVisual: 'Hub, Worker, Control and WSS relay architecture',
        clickZoom: 'Click to inspect',
        cardAgentTitle: 'Agent-unaware',
        cardAgentBody: 'No agent SDK, callback server, or vendor-specific remote feature. AgentMux attaches below the agent at the shell/tmux layer.',
        cardCloudTitle: 'Cloudflare-ready',
        cardCloudBody: 'Run Hub behind Cloudflare Tunnel or a proxy. HTTPS becomes WSS, and workers keep outbound-only connectivity by default.',
        cardControlTitle: 'Multi-session control',
        cardControlBody: 'The Web Control surface supports resizable panes, drag placement, session creation, and browser credentials.',
        releaseEyebrow: 'GitHub release',
        releaseTitle: 'Latest version',
        releaseLead: 'The landing page reads the latest GitHub release and shows the version, notes, binary assets, and container image.',
        releaseOpen: 'Open release',
        releaseLoading: 'Loading latest release note from GitHub...',
        releaseUnavailable: 'Release information is not available yet. Open GitHub Releases for published versions and assets.',
        releasePublished: 'Published ',
        dockerImage: 'Docker image',
        visualTitle: 'Architecture in practice',
        visualLead: 'Large technical diagrams are embedded directly into the Hub landing page and can be opened for detail review.',
        onboardingTitle: 'Signal onboarding',
        onboardingBody: 'Generate a short-lived signal, exchange it for scoped credentials, then connect workers and controls.',
        cloudflareTitle: 'Cloudflare-ready deployment',
        cloudflareBody: 'Keep Hub and SQLite on your server while Cloudflare terminates HTTPS and WSS through a tunnel.',
        workspaceTitle: 'Multi-pane Web Control',
        workspaceBody: 'Operate multiple long-lived tmux-backed agent sessions from a compact browser workspace.',
        quickTitle: 'Quick Start',
        quickLead: 'Install the release binary through the generated script, then connect before the signal expires.',
        generate: 'Generate',
        installBinary: 'Install binary',
        cloudflareTunnel: 'Cloudflare tunnel',
        footerSecurity: 'Default admin token stays local. Signals exchange into scoped credentials.',
        footerDocs: 'Docs',
        generating: 'Generating signal...',
        failed: 'Failed: ',
        signalReady: 'Signal ready · expires ',
        signal: 'Signal',
        workerCommand: 'Worker install command',
        webControl: 'Web Control',
        controlCommand: 'Control app command',
        copy: 'Copy',
        copied: 'Copied'
      },
      zh: {
        navControl: '网页控制台',
        navQuick: '快速开始',
        navGithub: 'GitHub',
        eyebrow: '面向 coding agent 的开源 tmux 控制平面',
        heroTitle: '把所有 agent 会话收回到一个 Hub。',
        heroLead: 'AgentMux 让 Codex、Claude、Gemini、OpenCode 和普通 shell 完全无感。Worker 管理本地 tmux 会话，Hub 负责身份与 WebSocket 路由，Control 让你从浏览器或 CLI 在任意设备接管会话。',
        generateSignal: '生成接入信令',
        openControl: '打开网页控制台',
        openGithub: '在 GitHub 查看源码',
        heroVisual: 'Hub、Worker、Control 与 WSS 中继架构',
        clickZoom: '点击查看大图',
        cardAgentTitle: 'Agent 无感',
        cardAgentBody: '不依赖 agent SDK、回调服务或厂商远程能力。AgentMux 在 shell/tmux 层接入，agent 只看到本地终端。',
        cardCloudTitle: '适配 Cloudflare',
        cardCloudBody: 'Hub 可以放在 Cloudflare Tunnel 或反向代理之后。HTTPS 自动对应 WSS，Worker 默认只需要出站连接。',
        cardControlTitle: '多会话控制',
        cardControlBody: 'Web Control 支持可调整窗格、拖拽布局、会话创建，以及浏览器侧凭证。',
        releaseEyebrow: 'GitHub 版本发布',
        releaseTitle: '最新版本',
        releaseLead: '落地页会读取 GitHub 最新 release，并展示版本号、说明、二进制产物和容器镜像。',
        releaseOpen: '打开版本页',
        releaseLoading: '正在从 GitHub 加载最新 release note...',
        releaseUnavailable: '暂时无法获取版本信息。可以打开 GitHub Releases 查看已发布版本和产物。',
        releasePublished: '发布时间 ',
        dockerImage: 'Docker 镜像',
        visualTitle: '架构如何落地',
        visualLead: '关键技术图示直接嵌入 Hub 落地页，点击即可放大查看细节。',
        onboardingTitle: '信令接入流程',
        onboardingBody: '生成限时信令，交换为受限凭证，然后接入 Worker 与 Control。',
        cloudflareTitle: 'Cloudflare 部署形态',
        cloudflareBody: 'Hub 和 SQLite 留在自己的服务器上，由 Cloudflare 通过 Tunnel 承载 HTTPS 与 WSS。',
        workspaceTitle: '多窗格网页控制台',
        workspaceBody: '在紧凑的浏览器工作区里操作多个长期运行的 tmux agent 会话。',
        quickTitle: '快速开始',
        quickLead: '通过生成的脚本安装 release 二进制，然后在信令过期前完成接入。',
        generate: '生成',
        installBinary: '安装二进制',
        cloudflareTunnel: 'Cloudflare Tunnel',
        footerSecurity: '默认管理员 token 保持本地使用。信令会交换为受限凭证。',
        footerDocs: '文档',
        generating: '正在生成信令...',
        failed: '失败：',
        signalReady: '信令已就绪 · 过期时间 ',
        signal: '信令',
        workerCommand: 'Worker 安装命令',
        webControl: '网页控制台',
        controlCommand: 'Control 应用命令',
        copy: '复制',
        copied: '已复制'
      }
    };
    let currentLang = localStorage.getItem('agentmux.lang') || ((navigator.language || '').toLowerCase().startsWith('zh') ? 'zh' : 'en');
    if (!dictionaries[currentLang]) currentLang = 'en';
    document.getElementById('mint').addEventListener('click', mint);
    document.getElementById('mint2').addEventListener('click', mint);
    document.querySelectorAll('[data-lang]').forEach(button => {
      button.addEventListener('click', () => setLanguage(button.getAttribute('data-lang')));
    });
    document.querySelectorAll('[data-full]').forEach(button => {
      button.addEventListener('click', () => openLightbox(button.getAttribute('data-full'), button.getAttribute('data-title')));
    });
    document.getElementById('lightbox-close').addEventListener('click', closeLightbox);
    document.getElementById('lightbox').addEventListener('click', event => {
      if (event.target.id === 'lightbox') closeLightbox();
    });
    document.addEventListener('keydown', event => {
      if (event.key === 'Escape') closeLightbox();
    });
    setLanguage(currentLang);
    loadRelease();
    async function mint() {
      status.textContent = t('generating');
      const res = await fetch('/api/signals', { method: 'POST' });
      if (!res.ok) {
        status.textContent = t('failed') + await res.text();
        return;
      }
      const data = await res.json();
      const signal = data.signal || data.token;
      status.textContent = t('signalReady') + new Date(data.expires_at).toLocaleString();
      result.innerHTML =
        commandBlock(t('signal'), signal, false) +
        commandBlock(t('workerCommand'), data.worker_command, true) +
        commandBlock(t('webControl'), data.control_url, false) +
        commandBlock(t('controlCommand'), data.control_command, true);
    }
    function setLanguage(lang) {
      currentLang = dictionaries[lang] ? lang : 'en';
      localStorage.setItem('agentmux.lang', currentLang);
      document.documentElement.lang = currentLang === 'zh' ? 'zh-CN' : 'en';
      document.querySelectorAll('[data-i18n]').forEach(node => {
        const key = node.getAttribute('data-i18n');
        if (dictionaries[currentLang][key]) node.textContent = dictionaries[currentLang][key];
      });
      document.querySelectorAll('[data-lang]').forEach(button => {
        button.classList.toggle('active', button.getAttribute('data-lang') === currentLang);
      });
      if (status.textContent === '' || status.textContent.startsWith('Hub ')) status.textContent = baseStatus;
      renderRelease();
    }
    function t(key) {
      return dictionaries[currentLang][key] || dictionaries.en[key] || key;
    }
    function commandBlock(title, value, copyable) {
      return '<div class="command"><div class="command-title"><span>' + escapeHTML(title) + '</span>' +
        (copyable ? '<button data-copy="' + escapeAttr(value) + '" onclick="copyValue(this)">' + escapeHTML(t('copy')) + '</button>' : '') +
        '</div><pre>' + escapeHTML(value) + '</pre></div>';
    }
    async function copyValue(button) {
      await navigator.clipboard.writeText(button.getAttribute('data-copy') || '');
      button.textContent = t('copied');
      setTimeout(() => button.textContent = t('copy'), 1000);
    }
    function openLightbox(src, title) {
      document.getElementById('lightbox-title').textContent = title || 'Preview';
      const image = document.getElementById('lightbox-img');
      image.src = src;
      image.alt = title || 'Preview';
      document.getElementById('lightbox').classList.add('open');
    }
    function closeLightbox() {
      document.getElementById('lightbox').classList.remove('open');
    }
    async function loadRelease() {
      const note = document.getElementById('release-note');
      try {
        const response = await fetch(latestReleaseAPI, { headers: { 'Accept': 'application/vnd.github+json' } });
        if (!response.ok) throw new Error(String(response.status));
        latestRelease = await response.json();
        renderRelease();
      } catch {
        document.getElementById('release-version').textContent = 'GitHub Releases';
        document.getElementById('release-link').href = fallbackReleaseURL;
        note.textContent = t('releaseUnavailable');
      }
    }
    function renderRelease() {
      if (!latestRelease) return;
      const tag = latestRelease.tag_name || latestRelease.name || 'latest';
      const version = document.getElementById('release-version');
      const link = document.getElementById('release-link');
      const note = document.getElementById('release-note');
      const assets = document.getElementById('release-assets');
      const dockerCommand = document.getElementById('docker-command');
      version.textContent = tag + (latestRelease.published_at ? ' · ' + t('releasePublished') + new Date(latestRelease.published_at).toLocaleDateString() : '');
      link.href = latestRelease.html_url || fallbackReleaseURL;
      note.textContent = trimReleaseNote(latestRelease.body || latestRelease.name || t('releaseUnavailable'));
      dockerCommand.textContent = 'docker run --rm -p 8080:8080 -v agentmux-data:/var/lib/agentmux ' + containerImage + ':' + tag.replace(/^v/, '') + ' hub --addr 0.0.0.0:8080 --data /var/lib/agentmux/agentmux.db --public-url {{.BaseURL}}';
      assets.innerHTML = '';
      for (const asset of latestRelease.assets || []) {
        const href = asset.browser_download_url || asset.url;
        if (!href || !asset.name) continue;
        const item = document.createElement('a');
        item.href = href;
        item.rel = 'noreferrer';
        item.textContent = asset.name;
        assets.appendChild(item);
      }
    }
    function trimReleaseNote(value) {
      const text = String(value).trim();
      if (text.length <= 900) return text;
      return text.slice(0, 900).replace(/\s+\S*$/, '') + '...';
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
