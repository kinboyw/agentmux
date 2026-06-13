package hub

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
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
	buildversion "private/agentmux/internal/version"
	"private/agentmux/internal/ws"
)

const (
	wsPingInterval           = 15 * time.Second
	wsPongWait               = 45 * time.Second
	sessionAuxRequestWait    = 8 * time.Second
	sessionCreateRequestWait = 5 * time.Second
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
	workerViews map[string]workerRecord
	sessions    map[string]protocol.SessionView
	subscribers map[string]*controlConn
	streams     map[string]streamSubscription
	controls    map[string]*controlConn
	previews    map[string]chan protocol.Envelope
	targets     map[string]chan protocol.Envelope
	creates     map[string]chan protocol.Envelope
	updateJobs  map[string]*workerUpdateJob
	auth        AuthStore
	runtime     RuntimeStore
	rateLimiter *rateLimiter
}

type workerConn struct {
	id         string
	instanceID string
	tenantID   string
	name       string
	addr       string
	backend    string
	software   protocol.WorkerSoftware
	lastSeen   time.Time
	conn       *ws.Conn
	send       chan protocol.Envelope
	done       chan struct{}
	close      sync.Once
}

type workerRecord struct {
	id           string
	instanceID   string
	tenantID     string
	name         string
	addr         string
	backend      string
	software     protocol.WorkerSoftware
	lastSeen     time.Time
	connected    bool
	disabled     bool
	traceEnabled bool
	debugEnabled bool
}

type workerUpdateJob struct {
	ID                     string              `json:"id"`
	TenantID               string              `json:"tenant_id,omitempty"`
	WorkerID               string              `json:"worker_id"`
	WorkerInstanceID       string              `json:"worker_instance_id,omitempty"`
	TargetVersion          string              `json:"target_version"`
	Repo                   string              `json:"repo"`
	Status                 string              `json:"status"`
	Message                string              `json:"message,omitempty"`
	AllowDisruptiveRestart bool                `json:"allow_disruptive_restart,omitempty"`
	CreatedAt              time.Time           `json:"created_at"`
	UpdatedAt              time.Time           `json:"updated_at"`
	FinishedAt             time.Time           `json:"finished_at,omitempty"`
	Events                 []workerUpdateEvent `json:"events,omitempty"`
}

type workerUpdateEvent struct {
	ID               string    `json:"id"`
	JobID            string    `json:"job_id"`
	TenantID         string    `json:"tenant_id,omitempty"`
	WorkerID         string    `json:"worker_id"`
	WorkerInstanceID string    `json:"worker_instance_id,omitempty"`
	Status           string    `json:"status"`
	Message          string    `json:"message,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
}

type streamSubscription struct {
	controlID string
	workerID  string
	sessionID string
	name      string
	targetKey string
}

type streamCloseRequest struct {
	streamID     string
	subscription streamSubscription
}

type controlConn struct {
	id       string
	conn     *ws.Conn
	send     chan protocol.Envelope
	tenantID string
	admin    bool
	direct   bool
	done     chan struct{}
	close    sync.Once
}

type authContext struct {
	Admin      bool
	Credential credentialEntry
}

type authContextKey struct{}

type rateLimitBucket struct {
	Count     int
	ResetAt   time.Time
	UpdatedAt time.Time
}

type rateLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	items  map[string]rateLimitBucket
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{limit: limit, window: window, items: map[string]rateLimitBucket{}}
}

func (l *rateLimiter) Allow(key string) bool {
	if l == nil || l.limit <= 0 || l.window <= 0 {
		return true
	}
	now := time.Now().UTC()
	l.mu.Lock()
	defer l.mu.Unlock()
	for existingKey, bucket := range l.items {
		if now.After(bucket.ResetAt) {
			delete(l.items, existingKey)
		}
	}
	bucket := l.items[key]
	if bucket.ResetAt.IsZero() || now.After(bucket.ResetAt) {
		bucket = rateLimitBucket{ResetAt: now.Add(l.window)}
	}
	bucket.Count++
	bucket.UpdatedAt = now
	l.items[key] = bucket
	return bucket.Count <= l.limit
}

func New(addr, token string, logger *slog.Logger) *Server {
	server, err := NewWithOptions(ServerOptions{Addr: addr, Token: token, Logger: logger})
	if err != nil {
		panic(err)
	}
	return server
}

type ServerOptions struct {
	Addr         string
	Token        string
	PublicURL    string
	ReleaseRepo  string
	Logger       *slog.Logger
	AuthStore    AuthStore
	RuntimeStore RuntimeStore
}

func NewWithOptions(options ServerOptions) (*Server, error) {
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}
	auth := defaultAuthStore(options.AuthStore)
	runtime := defaultRuntimeStore(options.RuntimeStore, auth)
	server := &Server{
		addr:        options.Addr,
		token:       options.Token,
		publicURL:   strings.TrimRight(options.PublicURL, "/"),
		releaseRepo: defaultReleaseRepo(options.ReleaseRepo),
		logger:      logger,
		workers:     map[string]*workerConn{},
		workerViews: map[string]workerRecord{},
		sessions:    map[string]protocol.SessionView{},
		subscribers: map[string]*controlConn{},
		streams:     map[string]streamSubscription{},
		controls:    map[string]*controlConn{},
		previews:    map[string]chan protocol.Envelope{},
		targets:     map[string]chan protocol.Envelope{},
		creates:     map[string]chan protocol.Envelope{},
		updateJobs:  map[string]*workerUpdateJob{},
		auth:        auth,
		runtime:     runtime,
		rateLimiter: newRateLimiter(10, time.Minute),
	}
	if err := server.loadRuntimeState(); err != nil {
		return nil, err
	}
	return server, nil
}

func defaultAuthStore(store AuthStore) AuthStore {
	if store != nil {
		return store
	}
	return newAuthStore()
}

func defaultRuntimeStore(store RuntimeStore, auth AuthStore) RuntimeStore {
	if store != nil {
		return store
	}
	if runtime, ok := auth.(RuntimeStore); ok {
		return runtime
	}
	return newMemoryRuntimeStore()
}

func (s *Server) loadRuntimeState() error {
	workers, err := s.runtime.LoadWorkers()
	if err != nil {
		return fmt.Errorf("load worker registry: %w", err)
	}
	for _, worker := range workers {
		if worker.id == "" {
			continue
		}
		worker.connected = false
		s.workerViews[worker.id] = worker
	}
	jobs, err := s.runtime.LoadUpdateJobs()
	if err != nil {
		return fmt.Errorf("load worker update jobs: %w", err)
	}
	for i := range jobs {
		job := jobs[i]
		if job.ID == "" {
			continue
		}
		s.updateJobs[job.ID] = &job
	}
	return nil
}

func (s *Server) persistWorkerRecord(record workerRecord) {
	if s.runtime == nil || record.id == "" {
		return
	}
	if err := s.runtime.SaveWorker(record); err != nil {
		s.logger.Warn("persist worker registry failed", "worker", record.id, "error", err)
	}
}

func (s *Server) persistUpdateJob(job *workerUpdateJob) {
	if s.runtime == nil || job == nil || job.ID == "" {
		return
	}
	if err := s.runtime.SaveUpdateJob(*job); err != nil {
		s.logger.Warn("persist worker update job failed", "job", job.ID, "worker", job.WorkerID, "error", err)
	}
}

func (s *Server) appendUpdateEvent(job *workerUpdateJob, status, message string, now time.Time) {
	if job == nil || job.ID == "" {
		return
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	event := workerUpdateEvent{
		ID: "evt_" + randomID(), JobID: job.ID, TenantID: job.TenantID,
		WorkerID: job.WorkerID, WorkerInstanceID: job.WorkerInstanceID,
		Status: status, Message: message, CreatedAt: now,
	}
	job.Events = append(job.Events, event)
	if s.runtime == nil {
		return
	}
	if err := s.runtime.AppendUpdateEvent(event); err != nil {
		s.logger.Warn("persist worker update event failed", "job", job.ID, "worker", job.WorkerID, "status", status, "error", err)
	}
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
	mux.HandleFunc("/run.sh", s.handleRunScript)
	mux.HandleFunc("/device", s.handleDevicePage)
	mux.HandleFunc("/control", s.handleControlPage)
	mux.HandleFunc("/assets/", s.handleWebAssets)
	mux.HandleFunc("/docassets/", s.handleDocAssets)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/api/version", s.handleVersion)
	mux.HandleFunc("/api/signals", s.handleSignals)
	mux.HandleFunc("/api/join-tokens", s.handleJoinTokens)
	mux.HandleFunc("/api/exchange", s.handleExchange)
	mux.HandleFunc("/api/auth/register", s.handleAuthRegister)
	mux.HandleFunc("/api/auth/login", s.handleAuthLogin)
	mux.HandleFunc("/api/auth/me", s.requireAuth(s.handleAuthMe))
	mux.HandleFunc("/api/auth/refresh", s.handleAuthRefresh)
	mux.HandleFunc("/api/auth/device/start", s.handleDeviceStart)
	mux.HandleFunc("/api/auth/device/poll", s.handleDevicePoll)
	mux.HandleFunc("/api/auth/device/approve", s.handleDeviceApprove)
	mux.HandleFunc("/api/auth/device/approve-current", s.handleDeviceApproveCurrent)
	mux.HandleFunc("/api/auth/oauth/", s.handleAuthOAuth)
	mux.HandleFunc("/api/workers", s.requireRole("control", s.handleWorkers))
	mux.HandleFunc("/api/workers/", s.requireRole("control", s.handleWorkerAction))
	mux.HandleFunc("/api/sessions", s.requireRole("control", s.handleSessions))
	mux.HandleFunc("/api/sessions/", s.requireRole("control", s.handleSessionAction))
	mux.HandleFunc("/ws/worker", s.handleWorkerWS)
	mux.HandleFunc("/ws/control", s.handleControlWS)

	go s.anonymousWorkerGC(ctx)

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

func (s *Server) anonymousWorkerGC(ctx context.Context) {
	ticker := time.NewTicker(anonymousWorkerGCInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			reaped, err := s.reapExpiredAnonymousWorkers(now)
			if err != nil {
				s.logger.Warn("anonymous worker gc failed", "error", err)
				continue
			}
			if reaped > 0 {
				s.logger.Info("anonymous worker gc completed", "workers", reaped)
			}
		}
	}
}

func (s *Server) reapExpiredAnonymousWorkers(now time.Time) (int, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	s.mu.RLock()
	tenants := map[string]bool{}
	for _, record := range s.workerViews {
		if isAnonymousTenant(record.tenantID) {
			tenants[record.tenantID] = true
		}
	}
	s.mu.RUnlock()
	reaped := 0
	for tenantID := range tenants {
		if s.auth.HasActiveControlCredential(tenantID, now) {
			continue
		}
		removed := s.interruptAnonymousTenant(tenantID)
		if s.runtime != nil {
			if err := s.runtime.DeleteTenantRuntime(tenantID); err != nil {
				return reaped, err
			}
		}
		reaped += removed
		if removed > 0 {
			s.logger.Warn("anonymous tenant expired; workers interrupted", "tenant", tenantID, "workers", removed)
		}
	}
	return reaped, nil
}

func (s *Server) interruptAnonymousTenant(tenantID string) int {
	if !isAnonymousTenant(tenantID) {
		return 0
	}
	var workers []*workerConn
	var controls []*controlConn
	removed := 0
	s.mu.Lock()
	for workerID, record := range s.workerViews {
		if record.tenantID != tenantID {
			continue
		}
		removed++
		if worker := s.workers[workerID]; worker != nil {
			workers = append(workers, worker)
			delete(s.workers, workerID)
		}
		delete(s.workerViews, workerID)
		for sessionID := range s.sessions {
			if strings.HasPrefix(sessionID, workerID+"/") {
				delete(s.sessions, sessionID)
			}
		}
	}
	for streamID, control := range s.subscribers {
		if control != nil && control.tenantID == tenantID {
			controls = append(controls, control)
			delete(s.subscribers, streamID)
		}
	}
	for controlID, control := range s.controls {
		if control != nil && control.tenantID == tenantID {
			controls = append(controls, control)
			delete(s.controls, controlID)
		}
	}
	for jobID, job := range s.updateJobs {
		if job != nil && job.TenantID == tenantID {
			delete(s.updateJobs, jobID)
		}
	}
	s.mu.Unlock()
	for _, worker := range workers {
		closeWorkerConn(worker)
	}
	for _, control := range controls {
		closeControlConn(control)
	}
	return removed
}

func closeWorkerConn(worker *workerConn) {
	if worker == nil {
		return
	}
	worker.close.Do(func() {
		if worker.done != nil {
			close(worker.done)
		}
	})
	if worker.conn != nil {
		_ = worker.conn.Close()
	}
}

func closeControlConn(control *controlConn) {
	if control == nil {
		return
	}
	control.close.Do(func() {
		if control.done != nil {
			close(control.done)
		}
	})
	if control.conn != nil {
		_ = control.conn.Close()
	}
}

func isAnonymousTenant(tenantID string) bool {
	return strings.HasPrefix(strings.TrimSpace(tenantID), "anon_")
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	info := buildversion.Get("hub")
	writeJSON(w, http.StatusOK, map[string]any{
		"role":             info.Role,
		"version":          info.Version,
		"commit":           info.Commit,
		"build_time":       info.BuildTime,
		"go_version":       info.GoVersion,
		"os":               info.OS,
		"arch":             info.Arch,
		"protocol_version": protocol.ProtocolVersion,
		"capabilities": []string{
			"api.version",
			"worker.software_inventory",
			"terminal.snapshot.v1",
			"control.web",
			"control.device_login",
			"update.local",
			"worker.update.remote",
			"run.cached",
		},
		"compatibility": map[string]any{
			"worker_protocol":  map[string]string{"min": "1", "preferred": protocol.ProtocolVersion},
			"control_protocol": map[string]string{"min": "1", "preferred": protocol.ProtocolVersion},
		},
		"release_repo": s.releaseRepo,
	})
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

func (s *Server) handleRunScript(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	baseURL := s.requestBaseURL(r)
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	_, _ = fmt.Fprintf(w, runScriptTemplate, s.releaseRepo, baseURL, websocketBase(baseURL))
}

func (s *Server) handleJoinTokens(w http.ResponseWriter, r *http.Request) {
	s.handleSignals(w, r)
}

func (s *Server) handleSignals(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	token := bearerOrQueryToken(r)
	auth, ok := s.authenticateRole(r, "control")
	if !ok && token != "" {
		writeError(w, http.StatusUnauthorized, "invalid control credential")
		return
	}
	tenantID := ""
	if ok {
		if isDirectControl(auth) {
			writeError(w, http.StatusForbidden, "direct token cannot generate join signals")
			return
		}
		if !auth.Admin {
			tenantID = auth.Credential.TenantID
		}
	} else if !s.rateLimiter.Allow(remoteHost(r.RemoteAddr) + "|anonymous-signals") {
		writeError(w, http.StatusTooManyRequests, "too many anonymous signal requests")
		return
	}
	minted, err := s.auth.MintSignalForTenant(tenantID, defaultSignalTTL, 0, []string{"worker:join"})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	controlSignal, err := s.auth.MintSignalForTenant(minted.TenantID, defaultSignalTTL, 1, []string{"control:join"})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	controlCredential, err := s.auth.Exchange(exchangeRequest{
		Signal: controlSignal.Signal, Role: "control", DeviceName: "direct-share",
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	baseURL := s.requestBaseURL(r)
	controlShareURL := baseURL + "/control?token=" + url.QueryEscape(controlCredential.Credential)
	writeJSON(w, http.StatusCreated, map[string]any{
		"token":                   minted.Signal,
		"signal":                  minted.Signal,
		"signal_id":               minted.ID,
		"tenant_id":               minted.TenantID,
		"expires_at":              minted.ExpiresAt,
		"uses_remaining":          minted.UsesRemaining,
		"reusable":                minted.UsesRemaining < 0,
		"scopes":                  minted.Scopes,
		"direct_token":            controlCredential.Credential,
		"direct_token_id":         controlCredential.CredentialID,
		"direct_token_expires_at": controlCredential.ExpiresAt,
		"control_share_url":       controlShareURL,
		"worker_command":          installWorkerCommand(baseURL, minted.Signal),
		"worker_join_command":     workerJoinCommand(baseURL, minted.Signal),
		"control_command":         installControlCommand(baseURL),
		"control_direct_command":  installControlDirectCommand(baseURL, controlCredential.Credential),
		"control_url":             baseURL + "/control",
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

func (s *Server) handleAuthMe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	auth := requestAuth(r)
	payload := map[string]any{
		"access_mode":   controlAccessMode(auth),
		"role":          auth.Credential.Role,
		"tenant_id":     auth.Credential.TenantID,
		"device_id":     auth.Credential.DeviceID,
		"credential_id": auth.Credential.ID,
		"expires_at":    auth.Credential.ExpiresAt,
	}
	if auth.Admin {
		payload["role"] = "admin"
		payload["user"] = authUserView{Name: "Admin"}
	} else if auth.Credential.UserEmail != "" {
		payload["user"] = authUserView{
			TenantID: auth.Credential.TenantID,
			Email:    auth.Credential.UserEmail,
			Name:     auth.Credential.Name,
		}
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *Server) handleAuthRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req refreshRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	credential, err := s.auth.Refresh(req)
	if err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, credential)
}

func (s *Server) handleDeviceStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req deviceStartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	response, err := s.auth.StartDeviceAuth(req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	baseURL := s.requestBaseURL(r)
	response.VerificationURL = baseURL + "/device"
	response.VerificationURLComplete = baseURL + "/device?user_code=" + url.QueryEscape(response.UserCode)
	writeJSON(w, http.StatusCreated, response)
}

func (s *Server) handleDevicePoll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req devicePollRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	response, err := s.auth.PollDeviceAuth(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleDeviceApprove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req deviceApproveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	limitKey := remoteHost(r.RemoteAddr) + "|" + normalizeUserCode(req.UserCode)
	if !s.rateLimiter.Allow(limitKey) {
		writeError(w, http.StatusTooManyRequests, "too many authorization attempts")
		return
	}
	credential, err := s.auth.ApproveDeviceAuth(req)
	if err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, credential)
}

func (s *Server) handleDeviceApproveCurrent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	auth, ok := s.authenticateRole(r, "control")
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if auth.Credential.UserEmail == "" {
		writeError(w, http.StatusForbidden, "current credential is not associated with a user")
		return
	}
	var req struct {
		UserCode string `json:"user_code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	limitKey := remoteHost(r.RemoteAddr) + "|" + normalizeUserCode(req.UserCode)
	if !s.rateLimiter.Allow(limitKey) {
		writeError(w, http.StatusTooManyRequests, "too many authorization attempts")
		return
	}
	credential, err := s.auth.ApproveDeviceAuthForUser(auth.Credential.UserEmail, req.UserCode)
	if err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, credential)
}

func (s *Server) handleDevicePage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	data := struct {
		UserCode string
		Info     deviceAuthInfo
		HasInfo  bool
	}{
		UserCode: normalizeUserCode(r.URL.Query().Get("user_code")),
	}
	if data.UserCode != "" {
		data.Info, data.HasInfo = s.auth.DeviceAuthInfo(data.UserCode)
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = deviceTemplate.Execute(w, data)
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
	workers := make([]protocol.WorkerView, 0, len(s.workerViews))
	for _, worker := range s.workerViews {
		if !auth.Admin && worker.tenantID != auth.Credential.TenantID {
			continue
		}
		workers = append(workers, workerView(worker))
	}
	s.mu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{"workers": workers})
}

type workerPatchRequest struct {
	Enabled      *bool `json:"enabled"`
	TraceEnabled *bool `json:"trace_enabled"`
	DebugEnabled *bool `json:"debug_enabled"`
}

type workerUpdateRequest struct {
	Version                string `json:"version"`
	Repo                   string `json:"repo"`
	AllowDisruptiveRestart bool   `json:"allow_disruptive_restart"`
}

func (s *Server) handleWorkerAction(w http.ResponseWriter, r *http.Request) {
	auth := requestAuth(r)
	if isDirectControl(auth) {
		writeError(w, http.StatusForbidden, "direct token cannot manage workers")
		return
	}
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/workers/"), "/")
	if path == "" {
		writeError(w, http.StatusNotFound, "worker path must be /api/workers/{id}")
		return
	}
	parts := strings.Split(path, "/")
	if len(parts) == 2 && parts[1] == "updates" {
		workerID, err := url.PathUnescape(parts[0])
		if err != nil || strings.TrimSpace(workerID) == "" || strings.Contains(workerID, "/") {
			writeError(w, http.StatusBadRequest, "invalid worker id")
			return
		}
		s.handleWorkerUpdates(w, r, auth, workerID)
		return
	}
	if len(parts) != 1 || r.Method != http.MethodPatch && r.Method != http.MethodDelete {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	workerID, err := url.PathUnescape(parts[0])
	if err != nil || strings.TrimSpace(workerID) == "" || strings.Contains(workerID, "/") {
		writeError(w, http.StatusBadRequest, "invalid worker id")
		return
	}

	if r.Method == http.MethodDelete {
		view, err := s.evictWorker(workerID, auth)
		if err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, errWorkerNotFound) {
				status = http.StatusNotFound
			} else if errors.Is(err, errWorkerForbidden) {
				status = http.StatusForbidden
			}
			writeError(w, status, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"worker": view, "status": "evicted"})
		return
	}

	var req workerPatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	s.mu.Lock()
	record := s.workerViews[workerID]
	if record.id == "" {
		s.mu.Unlock()
		writeError(w, http.StatusNotFound, "worker not found")
		return
	}
	if !auth.Admin && record.tenantID != auth.Credential.TenantID {
		s.mu.Unlock()
		writeError(w, http.StatusForbidden, "worker is not in credential tenant")
		return
	}
	if req.Enabled != nil {
		record.disabled = !*req.Enabled
	}
	if req.TraceEnabled != nil {
		record.traceEnabled = *req.TraceEnabled
	}
	if req.DebugEnabled != nil {
		record.debugEnabled = *req.DebugEnabled
	}
	s.workerViews[workerID] = record
	view := workerView(record)
	s.mu.Unlock()
	s.persistWorkerRecord(record)

	writeJSON(w, http.StatusOK, map[string]any{"worker": view})
}

func (s *Server) handleWorkerUpdates(w http.ResponseWriter, r *http.Request, auth authContext, workerID string) {
	switch r.Method {
	case http.MethodGet:
		s.mu.RLock()
		jobs := make([]workerUpdateJob, 0)
		for _, job := range s.updateJobs {
			if job.WorkerID != workerID {
				continue
			}
			if !auth.Admin && job.TenantID != auth.Credential.TenantID {
				continue
			}
			jobs = append(jobs, *job)
		}
		s.mu.RUnlock()
		writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs})
	case http.MethodPost:
		var req workerUpdateRequest
		if r.Body != nil {
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		if req.Version == "" {
			req.Version = "latest"
		}
		if req.Repo == "" {
			req.Repo = s.releaseRepo
		}
		job, err := s.startWorkerUpdate(workerID, auth, req)
		if err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, errWorkerNotFound) {
				status = http.StatusNotFound
			} else if errors.Is(err, errWorkerForbidden) {
				status = http.StatusForbidden
			} else if errors.Is(err, errWorkerUnavailable) || errors.Is(err, errWorkerCapabilityMissing) || errors.Is(err, errWorkerDisruptiveUpdate) {
				status = http.StatusConflict
			}
			writeError(w, status, err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"job": job})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) startWorkerUpdate(workerID string, auth authContext, req workerUpdateRequest) (workerUpdateJob, error) {
	now := time.Now().UTC()
	s.mu.Lock()
	record := s.workerViews[workerID]
	if record.id == "" {
		s.mu.Unlock()
		return workerUpdateJob{}, errWorkerNotFound
	}
	if !auth.Admin && record.tenantID != auth.Credential.TenantID {
		s.mu.Unlock()
		return workerUpdateJob{}, errWorkerForbidden
	}
	worker := s.workers[workerID]
	if worker == nil || !record.connected {
		s.mu.Unlock()
		return workerUpdateJob{}, errWorkerUnavailable
	}
	if record.disabled {
		s.mu.Unlock()
		return workerUpdateJob{}, fmt.Errorf("%w: worker is disabled", errWorkerUnavailable)
	}
	if !hasCapability(record.software, "worker.update.apply") {
		s.mu.Unlock()
		return workerUpdateJob{}, errWorkerCapabilityMissing
	}
	if !strings.EqualFold(record.backend, "tmux") && !req.AllowDisruptiveRestart {
		s.mu.Unlock()
		return workerUpdateJob{}, fmt.Errorf("%w: backend %s", errWorkerDisruptiveUpdate, record.backend)
	}
	job := &workerUpdateJob{
		ID: "upd_" + randomID(), TenantID: record.tenantID, WorkerID: workerID, WorkerInstanceID: record.instanceID,
		TargetVersion: req.Version, Repo: req.Repo, Status: "queued",
		AllowDisruptiveRestart: req.AllowDisruptiveRestart,
		CreatedAt:              now, UpdatedAt: now,
	}
	s.appendUpdateEvent(job, job.Status, "update queued", now)
	s.persistUpdateJob(job)
	s.updateJobs[job.ID] = job
	s.mu.Unlock()

	payload := protocol.WorkerUpdateApply{
		JobID: job.ID, Repo: req.Repo, Version: req.Version, Role: "worker",
		Restart: true, AllowDisruptiveRestart: req.AllowDisruptiveRestart,
	}
	if err := s.sendToWorkerWithID(workerID, protocol.TypeWorkerUpdateApply, payload, "", "", job.ID); err != nil {
		s.mu.Lock()
		job.Status = "failed"
		job.Message = err.Error()
		job.UpdatedAt = time.Now().UTC()
		job.FinishedAt = job.UpdatedAt
		s.appendUpdateEvent(job, job.Status, job.Message, job.UpdatedAt)
		s.persistUpdateJob(job)
		copy := *job
		s.mu.Unlock()
		return copy, err
	}
	s.mu.Lock()
	job.Status = "sent"
	job.Message = "update command sent"
	job.UpdatedAt = time.Now().UTC()
	s.appendUpdateEvent(job, job.Status, job.Message, job.UpdatedAt)
	s.persistUpdateJob(job)
	copy := *job
	s.mu.Unlock()
	return copy, nil
}

var (
	errWorkerNotFound          = errors.New("worker not found")
	errWorkerForbidden         = errors.New("worker is not in credential tenant")
	errWorkerUnavailable       = errors.New("worker is not connected")
	errWorkerCapabilityMissing = errors.New("worker does not support remote update")
	errWorkerDisruptiveUpdate  = errors.New("worker update requires disruptive restart confirmation")
)

func (s *Server) evictWorker(workerID string, auth authContext) (protocol.WorkerView, error) {
	s.mu.Lock()
	record := s.workerViews[workerID]
	if record.id == "" {
		s.mu.Unlock()
		return protocol.WorkerView{}, errWorkerNotFound
	}
	if !auth.Admin && record.tenantID != auth.Credential.TenantID {
		s.mu.Unlock()
		return protocol.WorkerView{}, errWorkerForbidden
	}
	record.disabled = true
	record.connected = false
	record.lastSeen = time.Now().UTC()
	worker := s.workers[workerID]
	delete(s.workers, workerID)
	staleStreams := s.removeWorkerStreamsLocked(workerID)
	for id := range s.sessions {
		if strings.HasPrefix(id, workerID+"/") {
			delete(s.sessions, id)
		}
	}
	s.workerViews[workerID] = record
	view := workerView(record)
	s.mu.Unlock()
	s.persistWorkerRecord(record)
	if len(staleStreams) > 0 {
		s.logger.Debug("worker eviction removed streams", "worker", workerID, "streams", len(staleStreams))
	}

	if worker != nil {
		worker.close.Do(func() {
			if worker.done != nil {
				close(worker.done)
			}
		})
		if worker.conn != nil {
			_ = worker.conn.Close()
		}
	}
	s.logger.Warn("worker evicted", "worker", workerID)
	return view, nil
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
		if !s.workerEnabled(req.WorkerID) {
			writeError(w, http.StatusForbidden, "worker is disabled")
			return
		}
		if err := s.requestSessionCreate(r.Context(), req.WorkerID, protocol.Session{
			Name: req.Name, CWD: req.CWD, Command: req.Command,
		}); err != nil {
			status := http.StatusBadGateway
			var rejected sessionCreateRejected
			if errors.As(err, &rejected) {
				status = http.StatusBadRequest
			} else if strings.Contains(err.Error(), "worker not connected") || strings.Contains(err.Error(), "worker send queue full") {
				status = http.StatusNotFound
			}
			writeError(w, status, err.Error())
			return
		}
		go s.requestSessionSync(req.WorkerID)
		writeJSON(w, http.StatusCreated, map[string]string{"status": "created"})
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
		if isDirectControl(auth) {
			writeError(w, http.StatusForbidden, "direct token cannot stop sessions")
			return
		}
		if err := s.sendToWorker(workerID, protocol.TypeSessionKill, map[string]string{"name": name}, "", sessionID); err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		go s.requestSessionSync(workerID)
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued"})
		return
	}
	if r.Method == http.MethodGet && len(parts) == 3 && parts[2] == "preview" {
		if isDirectControl(auth) {
			writeError(w, http.StatusForbidden, "direct token cannot preview sessions")
			return
		}
		lines := safeQueryInt(r.URL.Query().Get("lines"), 80)
		preview, err := s.requestSessionPreview(r.Context(), workerID, name, sessionID, lines, terminalTargetFromQuery(r.URL.Query(), name))
		if err != nil {
			writeError(w, sessionActionErrorStatus(err), err.Error())
			return
		}
		writeJSON(w, http.StatusOK, preview)
		return
	}
	if r.Method == http.MethodGet && len(parts) == 3 && parts[2] == "targets" {
		if isDirectControl(auth) {
			writeError(w, http.StatusForbidden, "direct token cannot inspect session targets")
			return
		}
		targets, err := s.requestSessionTargets(r.Context(), workerID, name, sessionID)
		if err != nil {
			writeError(w, sessionActionErrorStatus(err), err.Error())
			return
		}
		writeJSON(w, http.StatusOK, targets)
		return
	}
	if r.Method == http.MethodPost && len(parts) == 3 && parts[2] == "input" {
		if isDirectControl(auth) {
			writeError(w, http.StatusForbidden, "direct token cannot send REST input")
			return
		}
		if !s.workerEnabled(workerID) {
			writeError(w, http.StatusForbidden, "worker is disabled")
			return
		}
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
		id:         first.WorkerID,
		instanceID: strings.TrimSpace(hello.InstanceID),
		tenantID:   authTenantID(auth),
		name:       hello.Name,
		addr:       addr,
		backend:    hello.Backend,
		software:   hello.WorkerSoftware,
		lastSeen:   time.Now().UTC(),
		conn:       conn,
		send:       make(chan protocol.Envelope, 64),
		done:       make(chan struct{}),
	}
	if !s.workerAllowedToConnect(worker) {
		_ = conn.Close()
		s.logger.Warn("disabled worker connection rejected", "worker", worker.id)
		return
	}
	if err := s.workerConnectionConflict(worker); err != nil {
		s.writeWorkerHandshakeError(conn, worker.id, err.Error())
		_ = conn.Close()
		s.logger.Warn("worker connection rejected", "worker", worker.id, "instance", worker.instanceID, "error", err)
		return
	}
	s.registerWorker(worker)
	defer s.unregisterWorker(worker)

	go s.writeLoop("worker", worker.id, conn, worker.send, worker.done)
	go pingLoop(conn, worker.done, wsPingInterval)
	_ = conn.SetReadTimeout(wsPongWait)
	for {
		env, err := readEnvelope(conn)
		if err != nil {
			s.logger.Debug("worker websocket read ended", "worker", worker.id, "error", err)
			return
		}
		s.logger.Debug("worker message received", "worker", worker.id, "type", env.Type, "session_id", env.SessionID, "stream_id", env.StreamID, "request_id", env.ID, "payload_bytes", len(env.Payload))
		s.handleWorkerMessage(worker, env)
	}
}

func (s *Server) workerAllowedToConnect(worker *workerConn) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	record := s.workerViews[worker.id]
	if record.id == "" {
		return true
	}
	return !record.disabled
}

func (s *Server) workerConnectionConflict(worker *workerConn) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	old := s.workers[worker.id]
	if old == nil {
		return nil
	}
	if old.instanceID == "" || worker.instanceID == "" || old.instanceID == worker.instanceID {
		return nil
	}
	return fmt.Errorf("worker_id %q is already connected by another worker instance; set a unique --id or run worker leave before joining this machine", worker.id)
}

func (s *Server) writeWorkerHandshakeError(conn *ws.Conn, workerID, message string) {
	env, _ := protocol.NewEnvelope(protocol.TypeError, protocol.ErrorPayload{Message: message})
	env.WorkerID = workerID
	_ = writeEnvelope(conn, env)
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
	control := &controlConn{id: "ctrl_" + randomID(), conn: conn, send: make(chan protocol.Envelope, 64), tenantID: auth.Credential.TenantID, admin: auth.Admin, direct: isDirectControl(auth), done: make(chan struct{})}
	s.logger.Debug("control websocket connected", "control", control.id, "tenant", control.tenantID, "admin", control.admin)
	s.addControl(control)
	defer s.removeControl(control)
	go s.writeLoop("control", control.id, conn, control.send, control.done)
	go pingLoop(conn, control.done, wsPingInterval)
	_ = conn.SetReadTimeout(wsPongWait)
	for {
		env, err := readEnvelope(conn)
		if err != nil {
			s.logger.Debug("control websocket read ended", "control", control.id, "error", err)
			return
		}
		s.logger.Debug("control message received", "control", control.id, "type", env.Type, "session_id", env.SessionID, "stream_id", env.StreamID, "payload_bytes", len(env.Payload))
		s.handleControlMessage(control, env)
	}
}

func (s *Server) registerWorker(worker *workerConn) {
	if worker.done == nil {
		worker.done = make(chan struct{})
	}
	var old *workerConn
	s.mu.Lock()
	old = s.workers[worker.id]
	if old != nil {
		old.close.Do(func() {
			if old.done != nil {
				close(old.done)
			}
		})
		if old.conn != nil {
			_ = old.conn.Close()
		}
		s.logger.Warn("worker connection replaced", "worker", worker.id)
	}
	staleStreams := s.removeWorkerStreamsLocked(worker.id)
	s.workers[worker.id] = worker
	previous := s.workerViews[worker.id]
	if worker.instanceID == "" {
		worker.instanceID = previous.instanceID
	}
	software := mergeWorkerSoftware(previous.software, worker.software)
	s.workerViews[worker.id] = workerRecord{
		id: worker.id, instanceID: worker.instanceID, tenantID: worker.tenantID, name: worker.name, addr: worker.addr,
		backend: worker.backend, software: software, lastSeen: worker.lastSeen, connected: true,
		disabled: previous.disabled, traceEnabled: previous.traceEnabled, debugEnabled: previous.debugEnabled,
	}
	record := s.workerViews[worker.id]
	s.completeWorkerUpdateOnReconnectLocked(worker.id, software)
	s.mu.Unlock()
	if old != nil && len(staleStreams) > 0 {
		s.logger.Debug("worker replacement removed streams", "worker", worker.id, "streams", len(staleStreams))
	}
	s.persistWorkerRecord(record)
	s.logger.Info("worker connected", "worker", worker.id)
}

func (s *Server) unregisterWorker(worker *workerConn) {
	if worker == nil {
		return
	}
	s.mu.Lock()
	current := s.workers[worker.id]
	if current != worker {
		s.mu.Unlock()
		return
	}
	worker.close.Do(func() {
		if worker.done != nil {
			close(worker.done)
		}
	})
	delete(s.workers, worker.id)
	record := s.workerViews[worker.id]
	if record.id == "" {
		record = workerRecord{id: worker.id, instanceID: worker.instanceID, tenantID: worker.tenantID, name: worker.name, addr: worker.addr}
	}
	record.connected = false
	record.lastSeen = time.Now().UTC()
	if worker.instanceID != "" {
		record.instanceID = worker.instanceID
	}
	if worker.name != "" {
		record.name = worker.name
	}
	if worker.addr != "" {
		record.addr = worker.addr
	}
	if worker.backend != "" {
		record.backend = worker.backend
	}
	record.software = mergeWorkerSoftware(record.software, worker.software)
	s.workerViews[worker.id] = record
	staleStreams := s.removeWorkerStreamsLocked(worker.id)
	s.mu.Unlock()
	if len(staleStreams) > 0 {
		s.logger.Debug("worker disconnect removed streams", "worker", worker.id, "streams", len(staleStreams))
	}
	s.persistWorkerRecord(record)
	s.logger.Info("worker disconnected", "worker", worker.id)
}

func (s *Server) handleWorkerMessage(worker *workerConn, env protocol.Envelope) {
	s.mu.Lock()
	worker.lastSeen = time.Now().UTC()
	if record := s.workerViews[worker.id]; record.id != "" {
		record.lastSeen = worker.lastSeen
		record.connected = true
		if worker.backend != "" {
			record.backend = worker.backend
		}
		record.software = mergeWorkerSoftware(record.software, worker.software)
		s.workerViews[worker.id] = record
	}
	s.mu.Unlock()
	switch env.Type {
	case protocol.TypeWorkerHeartbeat:
		return
	case protocol.TypeSessionSnapshot:
		var snapshot protocol.SessionSnapshot
		if err := env.DecodePayload(&snapshot); err != nil {
			return
		}
		s.logger.Debug("worker session snapshot", "worker", worker.id, "sessions", len(snapshot.Sessions))
		s.updateSessions(worker.id, snapshot.Sessions)
	case protocol.TypeTerminalOutput:
		s.logger.Debug("terminal output from worker", "worker", worker.id, "session_id", env.SessionID, "stream_id", env.StreamID, "payload_bytes", len(env.Payload))
		s.publish(env.StreamID, env)
	case protocol.TypeTerminalMode, protocol.TypeTerminalSnapshot, protocol.TypeTerminalStateReset, protocol.TypeTerminalDiff, protocol.TypeTerminalHistoryPage:
		s.logger.Debug("terminal state message from worker", "worker", worker.id, "type", env.Type, "session_id", env.SessionID, "stream_id", env.StreamID, "payload_bytes", len(env.Payload))
		s.publish(env.StreamID, env)
	case protocol.TypeSessionPreview:
		s.completePreview(env)
	case protocol.TypeSessionTargets:
		s.completeTargets(env)
	case protocol.TypeSessionCreated:
		s.completeCreate(env)
	case protocol.TypeWorkerUpdateResult:
		s.completeWorkerUpdateResult(env)
	case protocol.TypeError:
		if env.ID != "" {
			if s.completeCreate(env) {
				return
			}
			if s.completePreview(env) {
				return
			}
			if s.completeTargets(env) {
				return
			}
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
			CWD: session.CWD, Command: session.Command, Status: session.Status, Backend: sessionBackend(session, s.workerViews[workerID]),
		}
	}
	s.mu.Unlock()
}

func sessionBackend(session protocol.Session, worker workerRecord) string {
	if session.Backend != "" {
		return session.Backend
	}
	return worker.backend
}

func sessionActionErrorStatus(err error) int {
	if err == nil {
		return http.StatusOK
	}
	if errors.Is(err, context.Canceled) {
		return http.StatusRequestTimeout
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout
	}
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "timed out"):
		return http.StatusGatewayTimeout
	case strings.Contains(message, "worker not connected"), strings.Contains(message, "worker send queue full"):
		return http.StatusServiceUnavailable
	default:
		return http.StatusBadGateway
	}
}

func mergeWorkerSoftware(previous, next protocol.WorkerSoftware) protocol.WorkerSoftware {
	if next.Version == "" {
		next.Version = previous.Version
	}
	if next.Commit == "" {
		next.Commit = previous.Commit
	}
	if next.BuildTime == "" {
		next.BuildTime = previous.BuildTime
	}
	if next.GoVersion == "" {
		next.GoVersion = previous.GoVersion
	}
	if next.ProtocolVersion == "" {
		next.ProtocolVersion = previous.ProtocolVersion
	}
	if len(next.Capabilities) == 0 {
		next.Capabilities = previous.Capabilities
	}
	if next.OS == "" {
		next.OS = previous.OS
	}
	if next.Arch == "" {
		next.Arch = previous.Arch
	}
	if next.InstallKind == "" {
		next.InstallKind = previous.InstallKind
	}
	if next.ServiceBackend == "" {
		next.ServiceBackend = previous.ServiceBackend
	}
	if next.UpdateChannel == "" {
		next.UpdateChannel = previous.UpdateChannel
	}
	if next.UpdatePolicy == "" {
		next.UpdatePolicy = previous.UpdatePolicy
	}
	return next
}

func hasCapability(software protocol.WorkerSoftware, capability string) bool {
	for _, item := range software.Capabilities {
		if item == capability {
			return true
		}
	}
	return false
}

func (s *Server) completeWorkerUpdateResult(env protocol.Envelope) {
	var result protocol.WorkerUpdateResult
	if err := env.DecodePayload(&result); err != nil {
		return
	}
	jobID := result.JobID
	if jobID == "" {
		jobID = env.ID
	}
	if jobID == "" {
		return
	}
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	job := s.updateJobs[jobID]
	if job == nil {
		return
	}
	status := strings.ToLower(strings.TrimSpace(result.Status))
	switch status {
	case "started":
		job.Status = "running"
	case "restarting":
		job.Status = "restarting"
	case "failed", "rejected":
		job.Status = status
		job.FinishedAt = now
	case "succeeded":
		job.Status = "succeeded"
		job.FinishedAt = now
	default:
		if status != "" {
			job.Status = status
		}
	}
	if result.Message != "" {
		job.Message = result.Message
	}
	if result.Version != "" && result.Version != "latest" {
		job.TargetVersion = result.Version
	}
	job.UpdatedAt = now
	s.appendUpdateEvent(job, job.Status, job.Message, now)
	s.persistUpdateJob(job)
}

func (s *Server) completeWorkerUpdateOnReconnectLocked(workerID string, software protocol.WorkerSoftware) {
	now := time.Now().UTC()
	for _, job := range s.updateJobs {
		if job.WorkerID != workerID || !workerUpdateActive(job.Status) {
			continue
		}
		if !workerUpdateVersionMatches(job.TargetVersion, software.Version) {
			continue
		}
		job.Status = "succeeded"
		job.Message = "worker reconnected with version " + firstNonEmpty(software.Version, "unknown")
		job.UpdatedAt = now
		job.FinishedAt = now
		s.appendUpdateEvent(job, job.Status, job.Message, now)
		s.persistUpdateJob(job)
	}
}

func workerUpdateActive(status string) bool {
	switch strings.ToLower(status) {
	case "queued", "sent", "running", "restarting":
		return true
	default:
		return false
	}
}

func workerUpdateVersionMatches(target, actual string) bool {
	target = normalizeVersionString(target)
	actual = normalizeVersionString(actual)
	if target == "" {
		return actual != ""
	}
	if target == "latest" {
		return actual != ""
	}
	return actual == target
}

func normalizeVersionString(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "refs/tags/")
	return value
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func workerView(worker workerRecord) protocol.WorkerView {
	status := "offline"
	if worker.connected {
		status = "online"
	}
	return protocol.WorkerView{
		ID:           worker.id,
		InstanceID:   worker.instanceID,
		TenantID:     worker.tenantID,
		Name:         worker.name,
		Addr:         worker.addr,
		Backend:      worker.backend,
		Software:     worker.software,
		LastSeen:     worker.lastSeen,
		Status:       status,
		Online:       worker.connected,
		Enabled:      !worker.disabled,
		TraceEnabled: worker.traceEnabled,
		DebugEnabled: worker.debugEnabled,
	}
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
		if !s.workerEnabled(workerID) {
			sendError(control.send, env.SessionID, "worker is disabled")
			return
		}
		var open protocol.TerminalOpen
		if err := env.DecodePayload(&open); err != nil {
			sendError(control.send, env.SessionID, err.Error())
			return
		}
		if control.direct && open.Target != nil {
			sendError(control.send, env.SessionID, "direct token cannot open targeted panes")
			return
		}
		size := open.Size()
		s.logger.Debug("control open stream", "control", control.id, "worker", workerID, "session_id", env.SessionID, "stream_id", env.StreamID, "cols", size.Cols, "rows", size.Rows, "pane_id", terminalTargetPane(open.Target))
		staleStreams := s.addSubscriber(env.StreamID, control, streamSubscription{
			controlID: control.id,
			workerID:  workerID,
			sessionID: env.SessionID,
			name:      name,
			targetKey: terminalTargetKey(open.Target),
		})
		s.closeStreams(staleStreams, "control stream replaced")
		if err := s.sendToWorker(workerID, protocol.TypeTerminalOpen, open, name, env.SessionID, env.StreamID); err != nil {
			s.logger.Debug("control open forward failed", "control", control.id, "worker", workerID, "session_id", env.SessionID, "stream_id", env.StreamID, "error", err)
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
		s.logger.Debug("control input forward", "control", control.id, "worker", workerID, "session_id", env.SessionID, "stream_id", env.StreamID, "bytes", len(input.Data))
		if err := s.sendToWorker(workerID, protocol.TypeTerminalInput, input, name, env.SessionID, env.StreamID); err != nil {
			s.logger.Debug("control input forward failed", "control", control.id, "worker", workerID, "session_id", env.SessionID, "stream_id", env.StreamID, "error", err)
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
		s.logger.Debug("control resize forward", "control", control.id, "worker", workerID, "session_id", env.SessionID, "stream_id", env.StreamID, "cols", size.Cols, "rows", size.Rows)
		if err := s.sendToWorker(workerID, protocol.TypeTerminalResize, size, name, env.SessionID, env.StreamID); err != nil {
			s.logger.Debug("control resize forward failed", "control", control.id, "worker", workerID, "session_id", env.SessionID, "stream_id", env.StreamID, "error", err)
			sendError(control.send, env.SessionID, err.Error())
		}
	case protocol.TypeTerminalSizeSync:
		workerID, name, ok := protocol.SplitSessionID(env.SessionID)
		if !ok {
			sendError(control.send, env.SessionID, "invalid session_id")
			return
		}
		if !control.admin && !s.workerInTenant(workerID, control.tenantID) {
			sendError(control.send, env.SessionID, "worker is not in credential tenant")
			return
		}
		var size protocol.TerminalSizeSync
		if err := env.DecodePayload(&size); err != nil {
			sendError(control.send, env.SessionID, err.Error())
			return
		}
		s.logger.Debug("control size sync forward", "control", control.id, "worker", workerID, "session_id", env.SessionID, "stream_id", env.StreamID, "cols", size.Cols, "rows", size.Rows)
		if err := s.sendToWorker(workerID, protocol.TypeTerminalSizeSync, size, name, env.SessionID, env.StreamID); err != nil {
			s.logger.Debug("control size sync forward failed", "control", control.id, "worker", workerID, "session_id", env.SessionID, "stream_id", env.StreamID, "error", err)
			sendError(control.send, env.SessionID, err.Error())
		}
	case protocol.TypeTerminalSizeReset:
		workerID, name, ok := protocol.SplitSessionID(env.SessionID)
		if !ok {
			sendError(control.send, env.SessionID, "invalid session_id")
			return
		}
		if !control.admin && !s.workerInTenant(workerID, control.tenantID) {
			sendError(control.send, env.SessionID, "worker is not in credential tenant")
			return
		}
		var req protocol.TerminalSizeReset
		_ = env.DecodePayload(&req)
		s.logger.Debug("control size reset forward", "control", control.id, "worker", workerID, "session_id", env.SessionID, "stream_id", env.StreamID)
		if err := s.sendToWorker(workerID, protocol.TypeTerminalSizeReset, req, name, env.SessionID, env.StreamID); err != nil {
			s.logger.Debug("control size reset forward failed", "control", control.id, "worker", workerID, "session_id", env.SessionID, "stream_id", env.StreamID, "error", err)
			sendError(control.send, env.SessionID, err.Error())
		}
	case protocol.TypeTerminalHistoryReq:
		workerID, name, ok := protocol.SplitSessionID(env.SessionID)
		if !ok {
			sendError(control.send, env.SessionID, "invalid session_id")
			return
		}
		if !control.admin && !s.workerInTenant(workerID, control.tenantID) {
			sendError(control.send, env.SessionID, "worker is not in credential tenant")
			return
		}
		var req protocol.TerminalHistoryRequest
		if err := env.DecodePayload(&req); err != nil {
			sendError(control.send, env.SessionID, err.Error())
			return
		}
		if err := s.sendToWorker(workerID, protocol.TypeTerminalHistoryReq, req, name, env.SessionID, env.StreamID); err != nil {
			sendError(control.send, env.SessionID, err.Error())
		}
	case protocol.TypeTerminalMouse:
		workerID, name, ok := protocol.SplitSessionID(env.SessionID)
		if !ok {
			sendError(control.send, env.SessionID, "invalid session_id")
			return
		}
		if !control.admin && !s.workerInTenant(workerID, control.tenantID) {
			sendError(control.send, env.SessionID, "worker is not in credential tenant")
			return
		}
		var mouse protocol.TerminalMouse
		if err := env.DecodePayload(&mouse); err != nil {
			sendError(control.send, env.SessionID, err.Error())
			return
		}
		if err := s.sendToWorker(workerID, protocol.TypeTerminalMouse, mouse, name, env.SessionID, env.StreamID); err != nil {
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
		s.logger.Debug("control close forward", "control", control.id, "worker", workerID, "session_id", env.SessionID, "stream_id", env.StreamID)
		staleStreams := s.removeSubscriberStream(env.StreamID)
		if len(staleStreams) == 0 {
			staleStreams = []streamCloseRequest{{streamID: env.StreamID, subscription: streamSubscription{workerID: workerID, sessionID: env.SessionID, name: name}}}
		}
		s.closeStreams(staleStreams, "control close")
	}
}

func (s *Server) addControl(control *controlConn) {
	s.mu.Lock()
	s.controls[control.id] = control
	s.mu.Unlock()
}

func (s *Server) addSubscriber(streamID string, control *controlConn, subscription streamSubscription) []streamCloseRequest {
	s.mu.Lock()
	staleStreams := make([]streamCloseRequest, 0)
	for id, existing := range s.streams {
		if id == streamID {
			continue
		}
		if existing.controlID == control.id && existing.workerID == subscription.workerID && existing.sessionID == subscription.sessionID && existing.name == subscription.name && existing.targetKey == subscription.targetKey {
			delete(s.subscribers, id)
			delete(s.streams, id)
			staleStreams = append(staleStreams, streamCloseRequest{streamID: id, subscription: existing})
			s.logger.Debug("subscriber replaced", "control", control.id, "old_stream_id", id, "new_stream_id", streamID, "session_id", existing.sessionID)
		}
	}
	s.subscribers[streamID] = control
	s.streams[streamID] = subscription
	s.mu.Unlock()
	s.logger.Debug("subscriber added", "control", control.id, "stream_id", streamID)
	return staleStreams
}

func (s *Server) removeControl(control *controlConn) {
	control.close.Do(func() {
		if control.done != nil {
			close(control.done)
		}
	})
	s.mu.Lock()
	staleStreams := make([]streamCloseRequest, 0)
	for id, subscription := range s.streams {
		if subscription.controlID == control.id {
			delete(s.subscribers, id)
			delete(s.streams, id)
			staleStreams = append(staleStreams, streamCloseRequest{streamID: id, subscription: subscription})
			s.logger.Debug("subscriber removed", "control", control.id, "stream_id", id)
		}
	}
	delete(s.controls, control.id)
	s.mu.Unlock()
	s.closeStreams(staleStreams, "control disconnected")
	if control.conn != nil {
		_ = control.conn.Close()
	}
	s.logger.Debug("control websocket disconnected", "control", control.id)
}

func (s *Server) removeSubscriberStream(streamID string) []streamCloseRequest {
	if streamID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	subscription, ok := s.streams[streamID]
	delete(s.subscribers, streamID)
	delete(s.streams, streamID)
	if !ok {
		return nil
	}
	return []streamCloseRequest{{streamID: streamID, subscription: subscription}}
}

func (s *Server) removeWorkerStreams(workerID string) []streamCloseRequest {
	if workerID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.removeWorkerStreamsLocked(workerID)
}

func (s *Server) removeWorkerStreamsLocked(workerID string) []streamCloseRequest {
	staleStreams := make([]streamCloseRequest, 0)
	for streamID, subscription := range s.streams {
		if subscription.workerID != workerID {
			continue
		}
		delete(s.subscribers, streamID)
		delete(s.streams, streamID)
		staleStreams = append(staleStreams, streamCloseRequest{streamID: streamID, subscription: subscription})
		s.logger.Debug("worker stream removed", "worker", workerID, "stream_id", streamID, "session_id", subscription.sessionID)
	}
	return staleStreams
}

func (s *Server) closeStreams(streams []streamCloseRequest, reason string) {
	for _, stream := range streams {
		subscription := stream.subscription
		if subscription.workerID == "" || subscription.sessionID == "" || stream.streamID == "" {
			continue
		}
		s.logger.Debug("terminal close forwarded", "worker", subscription.workerID, "session_id", subscription.sessionID, "stream_id", stream.streamID, "reason", reason)
		_ = s.sendToWorker(subscription.workerID, protocol.TypeTerminalClose, map[string]string{"name": subscription.name, "reason": reason}, subscription.name, subscription.sessionID, stream.streamID)
	}
}

func (s *Server) publish(streamID string, env protocol.Envelope) {
	s.mu.RLock()
	control := s.subscribers[streamID]
	s.mu.RUnlock()
	if control == nil {
		s.logger.Debug("publish dropped without subscriber", "stream_id", streamID, "session_id", env.SessionID, "type", env.Type, "payload_bytes", len(env.Payload))
		s.closeOrphanStream(streamID, env.SessionID)
		return
	}
	select {
	case control.send <- env:
		s.logger.Debug("published to control", "control", control.id, "stream_id", streamID, "session_id", env.SessionID, "type", env.Type, "payload_bytes", len(env.Payload))
	default:
		s.logger.Debug("publish dropped control queue full", "control", control.id, "stream_id", streamID, "session_id", env.SessionID, "type", env.Type)
	}
}

func (s *Server) closeOrphanStream(streamID, sessionID string) {
	if streamID == "" || sessionID == "" {
		return
	}
	staleStreams := s.removeSubscriberStream(streamID)
	if len(staleStreams) > 0 {
		s.closeStreams(staleStreams, "orphan output")
		return
	}
	workerID, name, ok := protocol.SplitSessionID(sessionID)
	if !ok {
		return
	}
	s.closeStreams([]streamCloseRequest{{streamID: streamID, subscription: streamSubscription{workerID: workerID, sessionID: sessionID, name: name}}}, "orphan output")
}

func (s *Server) requestSessionPreview(ctx context.Context, workerID, name, sessionID string, lines int, target *protocol.TerminalTarget) (protocol.SessionPreview, error) {
	requestID := "preview_" + randomID()
	reply := make(chan protocol.Envelope, 1)
	s.logger.Debug("preview request start", "worker", workerID, "session_id", sessionID, "request_id", requestID, "lines", lines, "pane_id", terminalTargetPane(target))
	s.mu.Lock()
	s.previews[requestID] = reply
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.previews, requestID)
		s.mu.Unlock()
	}()

	scope := "active_pane"
	if target != nil && target.PaneID != "" {
		scope = "pane"
	}
	if err := s.sendToWorkerWithID(workerID, protocol.TypeSessionPreview, protocol.SessionPreviewRequest{Lines: lines, Scope: scope, Target: target}, name, sessionID, requestID); err != nil {
		s.logger.Debug("preview request forward failed", "worker", workerID, "session_id", sessionID, "request_id", requestID, "error", err)
		return protocol.SessionPreview{}, err
	}
	timer := time.NewTimer(sessionAuxRequestWait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return protocol.SessionPreview{}, ctx.Err()
	case <-timer.C:
		s.logger.Debug("preview request timed out", "worker", workerID, "session_id", sessionID, "request_id", requestID)
		return protocol.SessionPreview{}, fmt.Errorf("session preview timed out")
	case env := <-reply:
		if env.Type == protocol.TypeError {
			var payload protocol.ErrorPayload
			_ = env.DecodePayload(&payload)
			s.logger.Debug("preview request error", "worker", workerID, "session_id", sessionID, "request_id", requestID, "error", payload.Message)
			return protocol.SessionPreview{}, errors.New(payload.Message)
		}
		var preview protocol.SessionPreview
		if err := env.DecodePayload(&preview); err != nil {
			s.logger.Debug("preview response decode failed", "worker", workerID, "session_id", sessionID, "request_id", requestID, "error", err)
			return protocol.SessionPreview{}, err
		}
		if preview.Scope == "" {
			preview.Scope = "active_pane"
		}
		s.logger.Debug("preview request complete", "worker", workerID, "session_id", sessionID, "request_id", requestID, "bytes", len(preview.Data))
		return preview, nil
	}
}

func (s *Server) requestSessionTargets(ctx context.Context, workerID, name, sessionID string) (protocol.SessionTargets, error) {
	requestID := "targets_" + randomID()
	reply := make(chan protocol.Envelope, 1)
	s.logger.Debug("targets request start", "worker", workerID, "session_id", sessionID, "request_id", requestID)
	s.mu.Lock()
	s.targets[requestID] = reply
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.targets, requestID)
		s.mu.Unlock()
	}()

	if err := s.sendToWorkerWithID(workerID, protocol.TypeSessionTargets, protocol.SessionTargetsRequest{}, name, sessionID, requestID); err != nil {
		s.logger.Debug("targets request forward failed", "worker", workerID, "session_id", sessionID, "request_id", requestID, "error", err)
		return protocol.SessionTargets{}, err
	}
	timer := time.NewTimer(sessionAuxRequestWait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return protocol.SessionTargets{}, ctx.Err()
	case <-timer.C:
		s.logger.Debug("targets request timed out", "worker", workerID, "session_id", sessionID, "request_id", requestID)
		return protocol.SessionTargets{}, fmt.Errorf("session targets timed out")
	case env := <-reply:
		if env.Type == protocol.TypeError {
			var payload protocol.ErrorPayload
			_ = env.DecodePayload(&payload)
			s.logger.Debug("targets request error", "worker", workerID, "session_id", sessionID, "request_id", requestID, "error", payload.Message)
			return protocol.SessionTargets{}, errors.New(payload.Message)
		}
		var targets protocol.SessionTargets
		if err := env.DecodePayload(&targets); err != nil {
			s.logger.Debug("targets response decode failed", "worker", workerID, "session_id", sessionID, "request_id", requestID, "error", err)
			return protocol.SessionTargets{}, err
		}
		s.logger.Debug("targets request complete", "worker", workerID, "session_id", sessionID, "request_id", requestID, "targets", len(targets.Targets))
		return targets, nil
	}
}

func (s *Server) requestSessionSync(workerID string) {
	time.Sleep(150 * time.Millisecond)
	_ = s.sendToWorker(workerID, protocol.TypeSessionSync, nil, "", "")
}

type sessionCreateRejected struct {
	message string
}

func (e sessionCreateRejected) Error() string {
	return e.message
}

func (s *Server) requestSessionCreate(ctx context.Context, workerID string, session protocol.Session) error {
	requestID := "create_" + randomID()
	sessionID := protocol.SessionID(workerID, session.Name)
	reply := make(chan protocol.Envelope, 1)
	s.logger.Debug("session create request start", "worker", workerID, "session_id", sessionID, "request_id", requestID, "cwd", session.CWD, "command", session.Command)
	s.mu.Lock()
	s.creates[requestID] = reply
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.creates, requestID)
		s.mu.Unlock()
	}()

	if err := s.sendToWorkerWithID(workerID, protocol.TypeSessionCreate, session, session.Name, sessionID, requestID); err != nil {
		s.logger.Debug("session create request forward failed", "worker", workerID, "session_id", sessionID, "request_id", requestID, "error", err)
		return err
	}
	timer := time.NewTimer(sessionCreateRequestWait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		s.logger.Debug("session create request timed out", "worker", workerID, "session_id", sessionID, "request_id", requestID)
		return fmt.Errorf("session create timed out")
	case env := <-reply:
		if env.Type == protocol.TypeError {
			var payload protocol.ErrorPayload
			_ = env.DecodePayload(&payload)
			message := strings.TrimSpace(payload.Message)
			if message == "" {
				message = "worker rejected session create"
			}
			s.logger.Debug("session create request rejected", "worker", workerID, "session_id", sessionID, "request_id", requestID, "error", message)
			return sessionCreateRejected{message: message}
		}
		if env.Type != protocol.TypeSessionCreated {
			return fmt.Errorf("unexpected session create response: %s", env.Type)
		}
		s.logger.Debug("session create request complete", "worker", workerID, "session_id", sessionID, "request_id", requestID)
		return nil
	}
}

func (s *Server) completeCreate(env protocol.Envelope) bool {
	if env.ID == "" {
		return false
	}
	s.mu.RLock()
	reply := s.creates[env.ID]
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

func (s *Server) completeTargets(env protocol.Envelope) bool {
	if env.ID == "" {
		return false
	}
	s.mu.RLock()
	reply := s.targets[env.ID]
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

func terminalTargetPane(target *protocol.TerminalTarget) string {
	if target == nil {
		return ""
	}
	return target.PaneID
}

func terminalTargetKey(target *protocol.TerminalTarget) string {
	if target == nil {
		return ""
	}
	if target.PaneID != "" {
		return "pane_id:" + target.PaneID
	}
	if target.WindowID != "" || target.PaneIndex != 0 {
		return fmt.Sprintf("window_id:%s|pane_index:%d", target.WindowID, target.PaneIndex)
	}
	if target.WindowIndex != 0 || target.PaneIndex != 0 {
		return fmt.Sprintf("window_index:%d|pane_index:%d", target.WindowIndex, target.PaneIndex)
	}
	if target.WindowName != "" || target.PaneActive {
		return fmt.Sprintf("window_name:%s|pane_active:%t", target.WindowName, target.PaneActive)
	}
	return ""
}

func terminalTargetFromQuery(values url.Values, name string) *protocol.TerminalTarget {
	paneID := strings.TrimSpace(values.Get("pane_id"))
	if paneID == "" {
		return nil
	}
	return &protocol.TerminalTarget{
		SessionName:  firstNonEmptyString(values.Get("session_name"), name),
		WindowID:     strings.TrimSpace(values.Get("window_id")),
		WindowIndex:  safeQueryInt(values.Get("window_index"), 0),
		WindowName:   strings.TrimSpace(values.Get("window_name")),
		WindowActive: values.Get("window_active") == "true",
		PaneID:       paneID,
		PaneIndex:    safeQueryInt(values.Get("pane_index"), 0),
		PaneActive:   values.Get("pane_active") == "true",
		CWD:          strings.TrimSpace(values.Get("cwd")),
		Command:      strings.TrimSpace(values.Get("command")),
		Left:         safeQueryInt(values.Get("left"), 0),
		Top:          safeQueryInt(values.Get("top"), 0),
		Width:        safeQueryInt(values.Get("width"), 0),
		Height:       safeQueryInt(values.Get("height"), 0),
	}
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
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
		s.logger.Debug("queued message to worker", "worker", workerID, "type", messageType, "session_id", env.SessionID, "stream_id", env.StreamID, "request_id", requestID, "payload_bytes", len(raw))
		return nil
	default:
		s.logger.Debug("worker send queue full", "worker", workerID, "type", messageType, "session_id", env.SessionID, "stream_id", env.StreamID)
		return fmt.Errorf("worker send queue full: %s", workerID)
	}
}

func (s *Server) workerInTenant(workerID, tenantID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if worker := s.workers[workerID]; worker != nil {
		return worker.tenantID == tenantID
	}
	record := s.workerViews[workerID]
	return record.id != "" && record.tenantID == tenantID
}

func (s *Server) workerEnabled(workerID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	record := s.workerViews[workerID]
	return record.id == "" || !record.disabled
}

func (s *Server) workerTenantIDLocked(workerID string) string {
	worker := s.workers[workerID]
	if worker == nil {
		return s.workerViews[workerID].tenantID
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

func controlAccessMode(auth authContext) string {
	if auth.Admin {
		return "admin"
	}
	if auth.Credential.Role == "control" && auth.Credential.UserEmail == "" {
		return "direct"
	}
	if auth.Credential.Role == "control" {
		return "account"
	}
	return auth.Credential.Role
}

func isDirectControl(auth authContext) bool {
	return controlAccessMode(auth) == "direct"
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

func writeEnvelope(conn *ws.Conn, env protocol.Envelope) error {
	raw, err := json.Marshal(env)
	if err != nil {
		return err
	}
	return conn.WriteText(string(raw))
}

func (s *Server) writeLoop(peerType, peerID string, conn *ws.Conn, send <-chan protocol.Envelope, done <-chan struct{}) {
	for {
		select {
		case <-done:
			return
		case env := <-send:
			raw, err := json.Marshal(env)
			if err != nil {
				s.logger.Debug("websocket marshal failed", "peer_type", peerType, "peer", peerID, "type", env.Type, "session_id", env.SessionID, "stream_id", env.StreamID, "error", err)
				continue
			}
			if err := conn.WriteText(string(raw)); err != nil {
				s.logger.Debug("websocket write failed", "peer_type", peerType, "peer", peerID, "type", env.Type, "session_id", env.SessionID, "stream_id", env.StreamID, "error", err)
				return
			}
			s.logger.Debug("websocket message sent", "peer_type", peerType, "peer", peerID, "type", env.Type, "session_id", env.SessionID, "stream_id", env.StreamID, "request_id", env.ID, "bytes", len(raw))
		}
	}
}

func pingLoop(conn *ws.Conn, done <-chan struct{}, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			if err := conn.WritePing([]byte("agentmux")); err != nil {
				return
			}
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
HUB_BIN="$BIN_DIR/agentmux-hub"
TUI_BIN="$BIN_DIR/agentmux-tui"

mkdir -p "$BIN_DIR"

verify_sha256() {
  archive="$1"
  checksum_file="$2"
  expected="$(cut -d ' ' -f 1 "$checksum_file" | head -n 1)"
  if [ -z "$expected" ]; then
    echo "empty checksum file: $checksum_file" >&2
    exit 1
  fi
  if command -v sha256sum >/dev/null 2>&1; then
    actual="$(sha256sum "$archive" | cut -d ' ' -f 1)"
  elif command -v shasum >/dev/null 2>&1; then
    actual="$(shasum -a 256 "$archive" | cut -d ' ' -f 1)"
  else
    echo "sha256 verification requires sha256sum or shasum" >&2
    exit 1
  fi
  if [ "$actual" != "$expected" ]; then
    echo "sha256 mismatch for $archive" >&2
    echo "expected: $expected" >&2
    echo "actual:   $actual" >&2
    exit 1
  fi
}

case "$ROLE" in
  worker)
    ASSET_ROLE="worker"
    INSTALL_BIN="$BIN"
    ;;
  control)
    ASSET_ROLE="tui"
    INSTALL_BIN="$TUI_BIN"
    ;;
  hub)
    ASSET_ROLE="hub"
    INSTALL_BIN="$HUB_BIN"
    ;;
  *)
    echo "usage: install.sh worker|control|hub [agentmux flags]" >&2
    exit 2
    ;;
esac

if [ "$ROLE" = "control" ] && command -v agentmux-tui >/dev/null 2>&1; then
  INSTALL_BIN="$(command -v agentmux-tui)"
elif [ "$ROLE" = "hub" ] && command -v agentmux-hub >/dev/null 2>&1; then
  INSTALL_BIN="$(command -v agentmux-hub)"
elif [ "$ROLE" != "control" ] && command -v agentmux >/dev/null 2>&1; then
  INSTALL_BIN="$(command -v agentmux)"
elif command -v go >/dev/null 2>&1 && [ "$ROLE" = "hub" ] && [ -f "./cmd/agentmux-hub/main.go" ]; then
  go build -o "$INSTALL_BIN" ./cmd/agentmux-hub
elif command -v go >/dev/null 2>&1 && [ -f "./cmd/agentmux/main.go" ]; then
  go build -o "$INSTALL_BIN" ./cmd/agentmux
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
  asset_base="agentmux-${ASSET_ROLE}-${os}-${arch}"
  asset="${asset_base}.tar.gz"
  if [ "$VERSION" = "latest" ]; then
    url="https://github.com/${REPO}/releases/latest/download/${asset}"
  else
    url="https://github.com/${REPO}/releases/download/${VERSION}/${asset}"
  fi
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT
  echo "Downloading $url" >&2
  if ! curl -fsSL "$url" -o "$tmp/$asset"; then
    if [ "$ROLE" = "hub" ]; then
      exit 1
    fi
    if [ "$ROLE" = "control" ]; then
      fallback_base="agentmux-control-${os}-${arch}"
      fallback_asset="${fallback_base}.tar.gz"
      if [ "$VERSION" = "latest" ]; then
        fallback_url="https://github.com/${REPO}/releases/latest/download/${fallback_asset}"
      else
        fallback_url="https://github.com/${REPO}/releases/download/${VERSION}/${fallback_asset}"
      fi
      echo "Falling back to $fallback_url" >&2
      if curl -fsSL "$fallback_url" -o "$tmp/$fallback_asset"; then
        asset_base="$fallback_base"
        asset="$fallback_asset"
        url="$fallback_url"
      fi
    fi
    if [ ! -f "$tmp/$asset" ]; then
      legacy_base="agentmux-${os}-${arch}"
      asset_base="$legacy_base"
      asset="${legacy_base}.tar.gz"
      if [ "$VERSION" = "latest" ]; then
        url="https://github.com/${REPO}/releases/latest/download/${asset}"
      else
        url="https://github.com/${REPO}/releases/download/${VERSION}/${asset}"
      fi
      echo "Falling back to $url" >&2
      curl -fsSL "$url" -o "$tmp/$asset"
    fi
  fi
  checksum_url="${url}.sha256"
  echo "Verifying $checksum_url" >&2
  curl -fsSL "$checksum_url" -o "$tmp/$asset.sha256"
  verify_sha256 "$tmp/$asset" "$tmp/$asset.sha256"
  tar -xzf "$tmp/$asset" -C "$tmp"
  install -m 0755 "$tmp/${asset_base}" "$INSTALL_BIN"
else
  echo "agentmux binary is not installed." >&2
  echo "Install curl+tar, put agentmux in PATH, or run this script from a source checkout with Go installed." >&2
  exit 1
fi

case "$ROLE" in
  worker)
    exec "$INSTALL_BIN" worker join --hub "$HUB_WS" "$@"
    ;;
  control)
    exec "$INSTALL_BIN" --hub "$HUB_HTTP" "$@"
    ;;
  hub)
    exec "$INSTALL_BIN" hub "$@"
    ;;
esac
`

const runScriptTemplate = `#!/bin/sh
set -eu

TARGET="${1:-control@latest}"
shift || true

ROLE="${TARGET%%@*}"
VERSION="${TARGET#*@}"
if [ "$ROLE" = "$VERSION" ]; then
  VERSION="${AGENTMUX_VERSION:-latest}"
fi

REPO="${AGENTMUX_REPO:-%s}"
HUB_HTTP="%s"
HUB_WS="%s"
CACHE_DIR="${AGENTMUX_CACHE_DIR:-$HOME/.cache/agentmux}"

case "$ROLE" in
  worker|control|hub) ;;
  *)
    echo "usage: run.sh worker|control|hub[@version] [agentmux flags]" >&2
    exit 2
    ;;
esac

verify_sha256() {
  archive="$1"
  checksum_file="$2"
  expected="$(cut -d ' ' -f 1 "$checksum_file" | head -n 1)"
  if [ -z "$expected" ]; then
    echo "empty checksum file: $checksum_file" >&2
    exit 1
  fi
  if command -v sha256sum >/dev/null 2>&1; then
    actual="$(sha256sum "$archive" | cut -d ' ' -f 1)"
  elif command -v shasum >/dev/null 2>&1; then
    actual="$(shasum -a 256 "$archive" | cut -d ' ' -f 1)"
  else
    echo "sha256 verification requires sha256sum or shasum" >&2
    exit 1
  fi
  if [ "$actual" != "$expected" ]; then
    echo "sha256 mismatch for $archive" >&2
    echo "expected: $expected" >&2
    echo "actual:   $actual" >&2
    exit 1
  fi
}

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

asset_role="$ROLE"
if [ "$ROLE" = "control" ]; then
  asset_role="tui"
fi
asset_base="agentmux-${asset_role}-${os}-${arch}"
asset="${asset_base}.tar.gz"
case "$ROLE" in
  control) bin_name="agentmux-tui" ;;
  hub) bin_name="agentmux-hub" ;;
  *) bin_name="agentmux" ;;
esac
cache_bin="$CACHE_DIR/releases/$VERSION/$ROLE/${os}-${arch}/$bin_name"

if [ ! -x "$cache_bin" ]; then
  mkdir -p "$(dirname "$cache_bin")"
  if [ "$VERSION" = "latest" ]; then
    url="https://github.com/${REPO}/releases/latest/download/${asset}"
  else
    url="https://github.com/${REPO}/releases/download/${VERSION}/${asset}"
  fi
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT
  echo "Downloading $url" >&2
  if ! curl -fsSL "$url" -o "$tmp/$asset"; then
    if [ "$ROLE" = "hub" ]; then
      exit 1
    fi
    if [ "$ROLE" = "control" ]; then
      fallback_base="agentmux-control-${os}-${arch}"
      fallback_asset="${fallback_base}.tar.gz"
      if [ "$VERSION" = "latest" ]; then
        fallback_url="https://github.com/${REPO}/releases/latest/download/${fallback_asset}"
      else
        fallback_url="https://github.com/${REPO}/releases/download/${VERSION}/${fallback_asset}"
      fi
      echo "Falling back to $fallback_url" >&2
      if curl -fsSL "$fallback_url" -o "$tmp/$fallback_asset"; then
        asset_base="$fallback_base"
        asset="$fallback_asset"
        url="$fallback_url"
      fi
    fi
    if [ ! -f "$tmp/$asset" ]; then
      legacy_base="agentmux-${os}-${arch}"
      asset_base="$legacy_base"
      asset="${legacy_base}.tar.gz"
      if [ "$VERSION" = "latest" ]; then
        url="https://github.com/${REPO}/releases/latest/download/${asset}"
      else
        url="https://github.com/${REPO}/releases/download/${VERSION}/${asset}"
      fi
      echo "Falling back to $url" >&2
      curl -fsSL "$url" -o "$tmp/$asset"
    fi
  fi
  checksum_url="${url}.sha256"
  echo "Verifying $checksum_url" >&2
  curl -fsSL "$checksum_url" -o "$tmp/$asset.sha256"
  verify_sha256 "$tmp/$asset" "$tmp/$asset.sha256"
  tar -xzf "$tmp/$asset" -C "$tmp"
  install -m 0755 "$tmp/${asset_base}" "$cache_bin"
fi

case "$ROLE" in
  control)
    exec "$cache_bin" --hub "$HUB_HTTP" "$@"
    ;;
  worker)
    exec "$cache_bin" worker --hub "$HUB_WS" "$@"
    ;;
  hub)
    exec "$cache_bin" "$@"
    ;;
esac
`

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

var deviceTemplate = template.Must(template.New("device").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>AgentMux Device Login</title>
  <style>
    :root { color-scheme: dark; font-family: Inter, ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; background: #050607; color: #eef2f3; }
    body { margin: 0; min-height: 100vh; display: grid; place-items: center; background: radial-gradient(circle at 50% 0%, rgba(53,201,143,.18), transparent 34%), #050607; }
    main { width: min(420px, calc(100vw - 32px)); border: 1px solid rgba(255,255,255,.12); border-radius: 8px; background: rgba(13,16,18,.92); box-shadow: 0 24px 80px rgba(0,0,0,.45); }
    header { padding: 20px 22px 14px; border-bottom: 1px solid rgba(255,255,255,.1); }
    h1 { margin: 0; font-size: 18px; letter-spacing: 0; }
    p { margin: 8px 0 0; color: #9aa4aa; font-size: 13px; line-height: 1.5; }
    form { display: grid; gap: 12px; padding: 18px 22px 22px; }
    label { display: grid; gap: 6px; font-size: 12px; color: #9aa4aa; }
    input { height: 38px; border-radius: 6px; border: 1px solid rgba(255,255,255,.14); background: #07090a; color: #eef2f3; padding: 0 11px; font-size: 14px; outline: none; }
    input:focus { border-color: #35c98f; box-shadow: 0 0 0 2px rgba(53,201,143,.2); }
    button { height: 38px; border: 1px solid #35c98f; border-radius: 6px; background: #35c98f; color: #02110b; font-weight: 650; cursor: pointer; }
    .danger { border-color: rgba(255,94,94,.35); background: transparent; color: #ffb4b4; }
    .oauth-grid { display: grid; grid-template-columns: 1fr 1fr; gap: 10px; }
    .oauth { border-color: rgba(255,255,255,.14); background: #0b0e10; color: #eef2f3; font-weight: 600; }
    .divider { display: grid; grid-template-columns: 1fr auto 1fr; align-items: center; gap: 10px; color: #6f7a80; font-size: 11px; text-transform: uppercase; letter-spacing: .08em; }
    .divider::before, .divider::after { content: ""; height: 1px; background: rgba(255,255,255,.1); }
    .device-card { display: grid; gap: 8px; margin: 16px 22px 0; padding: 12px; border: 1px solid rgba(255,255,255,.12); border-radius: 8px; background: rgba(255,255,255,.035); }
    .row { display: flex; justify-content: space-between; gap: 14px; font-size: 12px; color: #9aa4aa; }
    .row strong { color: #eef2f3; font-weight: 650; text-align: right; overflow-wrap: anywhere; }
    .warning { margin: 0 22px; color: #d7b46a; font-size: 12px; line-height: 1.45; }
    .auth-panel { display: grid; gap: 12px; padding: 18px 22px 22px; }
    .account { display: grid; gap: 3px; padding: 12px; border: 1px solid rgba(53,201,143,.28); border-radius: 8px; background: rgba(53,201,143,.08); }
    .account strong { font-size: 14px; }
    .account span { color: #9aa4aa; font-size: 12px; overflow-wrap: anywhere; }
    .muted-button { border-color: rgba(255,255,255,.14); background: #0b0e10; color: #eef2f3; }
    .hidden { display: none !important; }
    #status { min-height: 18px; font-size: 12px; color: #9aa4aa; }
  </style>
</head>
<body>
  <main>
    <header>
      <h1>AgentMux device login</h1>
      <p>Confirm the code shown in your terminal, then sign in to authorize this Control device.</p>
    </header>
    {{if .HasInfo}}
    <section class="device-card">
      <div class="row"><span>Device</span><strong>{{.Info.DeviceName}}</strong></div>
      <div class="row"><span>Device ID</span><strong>{{.Info.DeviceID}}</strong></div>
      <div class="row"><span>Status</span><strong>{{.Info.Status}}</strong></div>
      <div class="row"><span>Expires</span><strong>{{.Info.ExpiresAt.Format "2006-01-02 15:04:05 UTC"}}</strong></div>
    </section>
    {{else}}
    <p class="warning">Only approve this request if you just started login from <strong>agentmux-tui</strong> or <strong>agentmux control login</strong> in your own terminal.</p>
    {{end}}
    <section id="current-auth" class="auth-panel hidden">
      <label>Code<input id="current_user_code" autocomplete="one-time-code" value="{{.UserCode}}" required></label>
      <div class="account">
        <strong id="current_name">Signed in</strong>
        <span id="current_email"></span>
      </div>
      <button id="approve-current" type="button">Authorize with this account</button>
      <button class="muted-button" id="use-other" type="button">Use another account</button>
      <button class="danger" id="deny-current" type="button">Deny request</button>
      <div id="current_status"></div>
    </section>
    <form id="form">
      <label>Code<input id="user_code" name="user_code" autocomplete="one-time-code" value="{{.UserCode}}" required></label>
      <div class="oauth-grid">
        <button class="oauth" type="button" data-provider="github">Continue with GitHub</button>
        <button class="oauth" type="button" data-provider="google">Continue with Google</button>
      </div>
      <div class="divider">or</div>
      <label>Email<input id="email" name="email" type="email" autocomplete="email" required></label>
      <label>Password<input id="password" name="password" type="password" autocomplete="current-password" required></label>
      <button type="submit">Authorize Control</button>
      <button class="danger" id="deny" type="button">Deny request</button>
      <div id="status"></div>
    </form>
  </main>
  <script>
    if (window.location.search) {
      window.history.replaceState({}, document.title, window.location.pathname);
    }
    const currentAuth = document.getElementById('current-auth');
    const form = document.getElementById('form');
    const currentStatus = document.getElementById('current_status');
    const status = document.getElementById('status');
    function readStoredUser() {
      try {
        return JSON.parse(localStorage.getItem('agentmux.user') || 'null');
      } catch {
        return null;
      }
    }
    function setOptionalStorage(key, value) {
      if (value) {
        localStorage.setItem(key, value);
      } else {
        localStorage.removeItem(key);
      }
    }
    function storeBrowserCredential(data) {
      if (!data || !data.credential) return;
      localStorage.setItem('agentmux.token', data.credential);
      setOptionalStorage('agentmux.token_expires_at', data.expires_at);
      setOptionalStorage('agentmux.refresh_token', data.refresh_token);
      setOptionalStorage('agentmux.refresh_expires_at', data.refresh_expires_at);
      if (data.user) {
        localStorage.setItem('agentmux.user', JSON.stringify(data.user));
      }
    }
    function clearBrowserCredential() {
      localStorage.removeItem('agentmux.token');
      localStorage.removeItem('agentmux.token_expires_at');
      localStorage.removeItem('agentmux.refresh_token');
      localStorage.removeItem('agentmux.refresh_expires_at');
      localStorage.removeItem('agentmux.user');
    }
    async function refreshBrowserCredential() {
      const refreshToken = localStorage.getItem('agentmux.refresh_token') || '';
      if (!refreshToken) return '';
      const res = await fetch('/api/auth/refresh', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ refresh_token: refreshToken })
      });
      if (!res.ok) {
        clearBrowserCredential();
        return '';
      }
      const data = await res.json();
      storeBrowserCredential(data);
      return data.credential || '';
    }
    function showPasswordLogin(message) {
      currentAuth.classList.add('hidden');
      form.classList.remove('hidden');
      if (message) status.textContent = message;
    }
    function showCurrentUser(user) {
      document.getElementById('current_name').textContent = user.name || user.email || 'Signed in';
      document.getElementById('current_email').textContent = user.email || '';
      currentAuth.classList.remove('hidden');
      form.classList.add('hidden');
    }
    function setButtonLoading(button, loading, label) {
      if (!button) return;
      if (loading) {
        if (!button.dataset.label) button.dataset.label = button.textContent;
        button.disabled = true;
        button.textContent = label || 'Working...';
      } else {
        button.disabled = false;
        button.textContent = button.dataset.label || button.textContent;
      }
    }
    function closeAfterSuccess(target) {
      let remaining = 5;
      const update = () => {
        target.textContent = 'Authorized. You can return to your terminal. This page will close in ' + remaining + 's.';
      };
      update();
      const timer = setInterval(() => {
        remaining -= 1;
        if (remaining <= 0) {
          clearInterval(timer);
          window.close();
          target.textContent = 'Authorized. You can close this page.';
          return;
        }
        update();
      }, 1000);
    }
    const storedUser = readStoredUser();
    if (storedUser && (localStorage.getItem('agentmux.token') || localStorage.getItem('agentmux.refresh_token'))) {
      showCurrentUser(storedUser);
    }
    document.querySelectorAll('[data-provider]').forEach(button => {
      button.addEventListener('click', () => {
        const provider = button.getAttribute('data-provider');
        status.textContent = provider + ' OAuth is reserved for the next auth milestone.';
      });
    });
    document.getElementById('use-other').addEventListener('click', () => {
      showPasswordLogin('Sign in with another account to authorize this device.');
    });
    document.getElementById('deny-current').addEventListener('click', () => {
      document.getElementById('current_user_code').value = '';
      currentStatus.textContent = 'Request denied locally. The terminal login will expire automatically.';
    });
    document.getElementById('deny').addEventListener('click', () => {
      document.getElementById('user_code').value = '';
      status.textContent = 'Request denied locally. The terminal login will expire automatically.';
    });
    async function approveWithCurrentCredential(accessToken) {
      return fetch('/api/auth/device/approve-current', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', 'Authorization': 'Bearer ' + accessToken },
        body: JSON.stringify({ user_code: document.getElementById('current_user_code').value })
      });
    }
    document.getElementById('approve-current').addEventListener('click', async event => {
      const button = event.currentTarget;
      setButtonLoading(button, true, 'Authorizing...');
      currentStatus.textContent = 'Authorizing...';
      try {
        let accessToken = localStorage.getItem('agentmux.token') || '';
        if (!accessToken) {
          accessToken = await refreshBrowserCredential();
        }
        if (!accessToken) {
          showPasswordLogin('Your browser session expired. Sign in again to authorize this device.');
          return;
        }
        let res = await approveWithCurrentCredential(accessToken);
        if (res.status === 401) {
          accessToken = await refreshBrowserCredential();
          if (accessToken) {
            res = await approveWithCurrentCredential(accessToken);
          }
        }
        if (!res.ok) {
          const detail = await res.text();
          if (res.status === 401 || res.status === 403) {
            showPasswordLogin(detail || 'Sign in again to authorize this device.');
            return;
          }
          currentStatus.textContent = detail;
          return;
        }
        closeAfterSuccess(currentStatus);
      } finally {
        setButtonLoading(button, false);
      }
    });
    document.getElementById('form').addEventListener('submit', async event => {
      event.preventDefault();
      const button = event.currentTarget.querySelector('button[type="submit"]');
      setButtonLoading(button, true, 'Authorizing...');
      status.textContent = 'Authorizing...';
      try {
        const body = {
          user_code: document.getElementById('user_code').value,
          email: document.getElementById('email').value,
          password: document.getElementById('password').value
        };
        const res = await fetch('/api/auth/device/approve', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(body)
        });
        if (!res.ok) {
          status.textContent = await res.text();
          return;
        }
        closeAfterSuccess(status);
      } finally {
        setButtonLoading(button, false);
      }
    });
  </script>
</body>
</html>`))

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
    button:disabled { cursor: wait; opacity: .72; transform: none; }
    .loading-spinner { width: 14px; height: 14px; border: 2px solid currentColor; border-right-color: transparent; border-radius: 999px; animation: spin .75s linear infinite; }
    .is-loading .loading-spinner { display: inline-block; }
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
    .version-link { border: 1px solid rgba(54, 214, 147, .34); border-radius: 999px; padding: 6px 9px; color: #d7f8ea; background: rgba(54, 214, 147, .1); font-weight: 650; }
    .lang { display: flex; align-items: center; gap: 4px; border: 1px solid var(--line); border-radius: 999px; padding: 3px; background: rgba(7, 10, 10, .72); }
    .lang button { min-height: 26px; border: 0; border-radius: 999px; padding: 0 9px; color: var(--muted); background: transparent; font-size: 12px; }
    .lang button.active { background: rgba(54, 214, 147, .18); color: var(--text); }
    main { width: min(1420px, calc(100% - 44px)); margin: 0 auto; }
    .hero { display: grid; grid-template-columns: minmax(0, .78fr) minmax(620px, 1.35fr); gap: 32px; align-items: center; padding: 62px 0 36px; }
    h1 { margin: 0; font-size: clamp(43px, 6.4vw, 84px); line-height: .95; letter-spacing: 0; max-width: 780px; }
    html[lang="zh-CN"] h1 { font-size: clamp(36px, 4.7vw, 64px); line-height: 1.08; max-width: 690px; }
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
    .visual-button { width: 100%; display: block; min-height: 0; padding: 0; border: 0; border-radius: 0; background: #050707; text-align: left; transform: none; }
    .visual-button:hover { transform: none; }
    .hero-visual img, .visual img { display: block; width: 100%; aspect-ratio: 16 / 9; height: auto; object-fit: contain; background: #050707; }
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
    .visual { display: grid; grid-template-columns: minmax(0, 1.7fr) minmax(260px, .55fr); overflow: hidden; transition: transform .18s ease, border-color .18s ease; }
    .visual:hover { transform: translateY(-2px); border-color: rgba(54, 214, 147, .42); }
    .visual:nth-child(even) { grid-template-columns: minmax(260px, .55fr) minmax(0, 1.7fr); }
    .visual:nth-child(even) .visual-copy { order: -1; border-left: 0; border-right: 1px solid var(--line); }
    .visual-copy { padding: 20px; border-left: 1px solid var(--line); display: flex; flex-direction: column; justify-content: center; }
    .visual h2 { margin: 0 0 8px; font-size: 19px; }
    .visual p { margin: 0; color: var(--muted); line-height: 1.55; font-size: 14px; }
    .visual .hint { margin-top: 14px; color: #cce7dc; font-size: 12px; }
    .panel { padding: 18px; margin-bottom: 52px; }
    .asset-list { display: flex; flex-wrap: wrap; gap: 7px; margin-top: 10px; }
    .asset-list a { border: 1px solid var(--line); border-radius: 999px; padding: 5px 8px; color: var(--muted); text-decoration: none; font-size: 12px; background: rgba(10, 14, 14, .72); }
    .panel-head { display: flex; align-items: center; justify-content: space-between; gap: 18px; margin-bottom: 14px; }
    .grid { display: grid; grid-template-columns: 1fr 1fr; gap: 14px; }
    .command { border: 1px solid var(--line); border-radius: 7px; background: rgba(3, 5, 5, .82); overflow: hidden; }
    .command-title { display: flex; align-items: center; justify-content: space-between; gap: 12px; border-bottom: 1px solid var(--line); color: var(--muted); font-size: 12px; padding: 8px 10px; }
    .command-actions { display: flex; align-items: center; gap: 7px; }
    .command-actions a, .command-actions button { min-height: 27px; border-radius: 6px; padding: 0 8px; font-size: 12px; }
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
    @keyframes spin {
      to { transform: rotate(360deg); }
    }
    @media (prefers-reduced-motion: reduce) {
      *, body::before { animation: none !important; transition: none !important; }
      html { scroll-behavior: auto; }
    }
    @media (max-width: 1000px) {
      .nav { padding: 0 18px; }
      .navlinks a.hide-small { display: none; }
      main { width: min(100% - 28px, 760px); }
      .hero, .grid, .cards, .visual, .visual:nth-child(even) { grid-template-columns: 1fr; }
      .hero { padding-top: 36px; }
      .hero-visual { transform: none; }
      .visual-copy, .visual:nth-child(even) .visual-copy { order: 0; border-left: 0; border-right: 0; border-top: 1px solid var(--line); }
      .section-head, .panel-head, .footer { align-items: flex-start; flex-direction: column; }
    }
    @media (max-width: 620px) {
      .navlinks { gap: 8px; }
      .navlinks a:not(.github-link):not(.version-link) { display: none; }
      h1 { font-size: 42px; }
      html[lang="zh-CN"] h1 { font-size: 34px; line-height: 1.12; }
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
        <a id="version-link" class="version-link" href="{{.ReleasesURL}}" rel="noreferrer">latest</a>
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
          <div class="pill"><span class="dot"></span><span data-i18n="eyebrow">Open-source terminal control plane for coding agents</span></div>
          <h1 data-i18n="heroTitle">Bring every agent session back to one hub.</h1>
          <p class="lead" data-i18n="heroLead">AgentMux keeps Codex, Claude, Gemini, OpenCode, and plain shells unaware of remote access. Workers own local tmux or PTY sessions, Hub routes identity and WebSockets, Control gives you browser and TUI access from anywhere.</p>
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
        <div class="card"><h2 data-i18n="cardAgentTitle">Agent-unaware</h2><p data-i18n="cardAgentBody">No agent SDK, callback server, or vendor-specific remote feature. AgentMux attaches below the agent at the terminal layer.</p></div>
        <div class="card"><h2 data-i18n="cardCloudTitle">Cloudflare-ready</h2><p data-i18n="cardCloudBody">Run Hub behind Cloudflare Tunnel or a proxy. HTTPS becomes WSS, and workers keep outbound-only connectivity by default.</p></div>
        <div class="card"><h2 data-i18n="cardControlTitle">Multi-session control</h2><p data-i18n="cardControlBody">Registered Control supports workspaces, previews, Worker management, and updates. Direct Token sharing stays limited to session list and terminal access.</p></div>
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
            <div class="visual-copy"><h2 data-i18n="onboardingTitle">Signal onboarding</h2><p data-i18n="onboardingBody">Generate a short-lived Worker signal plus a Direct Token share URL, then connect machines and anonymous Control devices.</p><span class="hint" data-i18n="clickZoom">Click to inspect</span></div>
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
            <div class="visual-copy"><h2 data-i18n="workspaceTitle">Multi-pane Web Control</h2><p data-i18n="workspaceBody">Operate multiple long-lived terminal sessions from a compact browser workspace.</p><span class="hint" data-i18n="clickZoom">Click to inspect</span></div>
          </article>
        </div>
      </section>

      <section id="quickstart" class="panel">
        <div class="panel-head">
          <div>
            <h2 data-i18n="quickTitle">Quick Start</h2>
            <p data-i18n="quickLead">This Hub is already running. Generate a join signal, run the Worker command on the machine that owns your sessions, then open the Web Control share URL or copy the Direct Token into TUI.</p>
          </div>
          <button id="mint2" data-i18n="generate">Generate</button>
        </div>
        <div id="result" class="grid">
          <div class="command">
            <div class="command-title"><span data-i18n="currentHub">Current Hub</span></div>
            <pre>{{.BaseURL}}</pre>
          </div>
          <div class="command">
            <div class="command-title"><span data-i18n="workerStep">Worker side</span></div>
            <pre data-i18n="workerStepBody">Click Generate to create a short-lived command for the machine running agents.</pre>
          </div>
          <div class="command">
            <div class="command-title"><span data-i18n="controlStep">Control side</span></div>
            <pre data-i18n="controlStepBody">After the Worker is connected, open the share URL for simple Direct Token access, or sign in for the full management workspace.</pre>
          </div>
        </div>
      </section>

      <section class="panel">
        <div class="panel-head">
          <div>
            <h2 data-i18n="selfHostTitle">Self-host Hub</h2>
            <p data-i18n="selfHostLead">Only use this when you want to run your own Hub. The Docker image already defaults to port 8081 and stores data under /var/lib/agentmux.</p>
          </div>
        </div>
        <div class="grid">
          <div class="command">
            <div class="command-title"><span data-i18n="runHubDocker">Run Hub with Docker</span></div>
            <pre>docker run -d --name agentmux --restart unless-stopped -p 8081:8081 {{.ContainerImage}}:latest</pre>
          </div>
          <div class="command">
            <div class="command-title"><span data-i18n="runHubBinary">Run Hub with binary</span></div>
            <pre>agentmux hub --addr 0.0.0.0:8081 --data ./agentmux.db</pre>
          </div>
          <div class="command">
            <div class="command-title"><span data-i18n="runHubPersist">Optional Docker persistence</span></div>
            <pre>docker run -d --name agentmux --restart unless-stopped -p 8081:8081 -v agentmux-data:/var/lib/agentmux {{.ContainerImage}}:latest</pre>
          </div>
          <div class="command">
            <div class="command-title"><span data-i18n="quickTunnel">Optional tunnel when Hub has no public URL</span></div>
            <pre>cloudflared tunnel --url http://127.0.0.1:8081
# open the printed https://*.trycloudflare.com URL and generate commands there</pre>
          </div>
        </div>
      </section>

      <footer class="footer">
        <span data-i18n="footerSecurity">Anonymous shares are scoped Direct Tokens. Registered Control accounts unlock Worker management, updates, previews, and workspaces.</span>
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
    const result = document.getElementById('result');
    const status = document.getElementById('status');
    const mintButtons = [document.getElementById('mint'), document.getElementById('mint2')].filter(Boolean);
    const quickStart = document.getElementById('quickstart');
    let latestRelease = null;
    let minting = false;
    const dictionaries = {
      en: {
        navControl: 'Web Control',
        navQuick: 'Quick Start',
        navGithub: 'GitHub',
        eyebrow: 'Open-source terminal control plane for coding agents',
        heroTitle: 'Bring every agent session back to one hub.',
        heroLead: 'AgentMux keeps Codex, Claude, Gemini, OpenCode, and plain shells unaware of remote access. Workers own local tmux or PTY sessions, Hub routes identity and WebSockets, Control gives you browser and TUI access from anywhere.',
        generateSignal: 'Generate join signal',
        openControl: 'Open Web Control',
        openGithub: 'Open-source on GitHub',
        heroVisual: 'Hub, Worker, Control and WSS relay architecture',
        clickZoom: 'Click to inspect',
        cardAgentTitle: 'Agent-unaware',
        cardAgentBody: 'No agent SDK, callback server, or vendor-specific remote feature. AgentMux attaches below the agent at the terminal layer.',
        cardCloudTitle: 'Cloudflare-ready',
        cardCloudBody: 'Run Hub behind Cloudflare Tunnel or a proxy. HTTPS becomes WSS, and workers keep outbound-only connectivity by default.',
        cardControlTitle: 'Multi-session control',
        cardControlBody: 'Registered Control supports workspaces, previews, Worker management, and updates. Direct Token sharing stays limited to session list and terminal access.',
        visualTitle: 'Architecture in practice',
        visualLead: 'Large technical diagrams are embedded directly into the Hub landing page and can be opened for detail review.',
        onboardingTitle: 'Signal onboarding',
        onboardingBody: 'Generate a short-lived Worker signal plus a Direct Token share URL, then connect machines and anonymous Control devices.',
        cloudflareTitle: 'Cloudflare-ready deployment',
        cloudflareBody: 'Keep Hub and SQLite on your server while Cloudflare terminates HTTPS and WSS through a tunnel.',
        workspaceTitle: 'Multi-pane Web Control',
        workspaceBody: 'Operate multiple long-lived terminal sessions from a compact browser workspace.',
        quickTitle: 'Quick Start',
        quickLead: 'This Hub is already running. Generate a join signal, run the Worker command on the machine that owns your sessions, then open the Web Control share URL or copy the Direct Token into TUI.',
        generate: 'Generate',
        currentHub: 'Current Hub',
        workerStep: 'Worker side',
        workerStepBody: 'Click Generate to create a short-lived command for the machine running agents.',
        controlStep: 'Control side',
        controlStepBody: 'After the Worker is connected, open the share URL for simple Direct Token access, or sign in for the full management workspace.',
        selfHostTitle: 'Self-host Hub',
        selfHostLead: 'Only use this when you want to run your own Hub. The Docker image already defaults to port 8081 and stores data under /var/lib/agentmux.',
        runHubBinary: 'Run Hub with binary',
        runHubDocker: 'Run Hub with Docker',
        runHubPersist: 'Optional Docker persistence',
        quickTunnel: 'Optional tunnel when Hub has no public URL',
        footerSecurity: 'Anonymous shares are scoped Direct Tokens. Registered Control accounts unlock Worker management, updates, previews, and workspaces.',
        footerDocs: 'Docs',
        generating: 'Generating signal...',
        failed: 'Failed: ',
        invalidControlToken: 'The saved control token is invalid or expired. Sign in again, apply a Direct Token, or clear the token to generate an anonymous share.',
        directTokenCannotGenerate: 'Direct Token access can only connect to shared sessions. Sign in with an account or open this Hub without a token to generate a new share.',
        rateLimited: 'Too many anonymous signal requests. Wait a minute and try again.',
        signalReady: 'Signal ready · expires ',
        signal: 'Signal',
        workerCommand: 'Worker install + join command',
        workerJoinCommand: 'Installed Worker join command',
        webControl: 'Web Control',
        directToken: 'Direct Token',
        webControlShare: 'Web Control share URL',
        controlDirectCommand: 'TUI Direct Token command',
        controlCommand: 'TUI Control command',
        open: 'Open',
        copy: 'Copy',
        copied: 'Copied'
      },
      zh: {
        navControl: '网页控制台',
        navQuick: '快速开始',
        navGithub: 'GitHub',
        eyebrow: '面向 coding agent 的开源终端控制平面',
        heroTitle: '把所有 agent 会话收回到一个 Hub。',
        heroLead: 'AgentMux 让 Codex、Claude、Gemini、OpenCode 和普通 shell 完全无感。Worker 管理本地 tmux 或 PTY 会话，Hub 负责身份与 WebSocket 路由，Control 让你从浏览器或 TUI 在任意设备接管会话。',
        generateSignal: '生成接入信令',
        openControl: '打开网页控制台',
        openGithub: '在 GitHub 查看源码',
        heroVisual: 'Hub、Worker、Control 与 WSS 中继架构',
        clickZoom: '点击查看大图',
        cardAgentTitle: 'Agent 无感',
        cardAgentBody: '不依赖 agent SDK、回调服务或厂商远程能力。AgentMux 在终端层接入，agent 只看到本地终端。',
        cardCloudTitle: '适配 Cloudflare',
        cardCloudBody: 'Hub 可以放在 Cloudflare Tunnel 或反向代理之后。HTTPS 自动对应 WSS，Worker 默认只需要出站连接。',
        cardControlTitle: '多会话控制',
        cardControlBody: '注册 Control 支持工作区、预览、Worker 管理和更新。Direct Token 分享只保留会话列表和终端连接。',
        visualTitle: '架构如何落地',
        visualLead: '关键技术图示直接嵌入 Hub 落地页，点击即可放大查看细节。',
        onboardingTitle: '信令接入流程',
        onboardingBody: '生成限时 Worker 信令和 Direct Token 分享链接，然后接入机器与匿名 Control 设备。',
        cloudflareTitle: 'Cloudflare 部署形态',
        cloudflareBody: 'Hub 和 SQLite 留在自己的服务器上，由 Cloudflare 通过 Tunnel 承载 HTTPS 与 WSS。',
        workspaceTitle: '多窗格网页控制台',
        workspaceBody: '在紧凑的浏览器工作区里操作多个长期运行的 tmux agent 会话。',
        quickTitle: '快速开始',
        quickLead: '当前 Hub 已经在运行。生成接入信令，在拥有会话的机器上运行 Worker 命令，然后打开 Web Control 分享链接，或把 Direct Token 复制到 TUI。',
        generate: '生成',
        currentHub: '当前 Hub',
        workerStep: 'Worker 侧',
        workerStepBody: '点击生成，为运行 agent 的机器创建一条限时 Worker 接入命令。',
        controlStep: 'Control 侧',
        controlStepBody: 'Worker 接入后，打开分享链接即可简单连接会话；登录账号后可使用完整管理工作区。',
        selfHostTitle: '自托管 Hub',
        selfHostLead: '只有当你想运行自己的 Hub 时才需要这里。Docker 镜像已默认使用 8081 端口，并把数据放在 /var/lib/agentmux。',
        runHubBinary: '用二进制运行 Hub',
        runHubDocker: '用 Docker 运行 Hub',
        runHubPersist: '可选 Docker 持久化',
        quickTunnel: 'Hub 无公网地址时可选穿透',
        footerSecurity: '匿名分享使用受限 Direct Token；注册 Control 账号可使用 Worker 管理、更新、预览和工作区。',
        footerDocs: '文档',
        generating: '正在生成信令...',
        failed: '失败：',
        invalidControlToken: '已保存的 control token 无效或已过期。请重新登录、输入 Direct Token，或清空 token 后生成匿名分享。',
        directTokenCannotGenerate: 'Direct Token 只能连接已分享的会话。请登录账号，或不带 token 打开 Hub 后再生成新的分享。',
        rateLimited: '匿名信令生成过于频繁，请稍等一分钟后再试。',
        signalReady: '信令已就绪 · 过期时间 ',
        signal: '信令',
        workerCommand: 'Worker 安装并接入命令',
        workerJoinCommand: '已安装 Worker 接入命令',
        webControl: '网页控制台',
        directToken: 'Direct Token 口令',
        webControlShare: '网页控制台分享链接',
        controlDirectCommand: 'TUI Direct Token 命令',
        controlCommand: 'TUI Control 命令',
        open: '打开',
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
    loadLatestVersion();
    async function mint() {
      if (minting) return;
      minting = true;
      setMintLoading(true);
      status.textContent = t('generating');
      try {
        const res = await fetch('/api/signals', { method: 'POST' });
        if (!res.ok) {
          status.textContent = signalErrorMessage(res.status, await res.text());
          return;
        }
        const data = await res.json();
        const signal = data.signal || data.token;
        status.textContent = t('signalReady') + new Date(data.expires_at).toLocaleString();
        result.innerHTML =
          commandBlock(t('signal'), signal, true) +
          commandBlock(t('workerCommand'), data.worker_command, true) +
          commandBlock(t('workerJoinCommand'), data.worker_join_command || '', true) +
          commandBlock(t('directToken'), data.direct_token || '', true) +
          linkBlock(t('webControlShare'), data.control_share_url || data.control_url) +
          commandBlock(t('controlDirectCommand'), data.control_direct_command || data.control_command, true);
        if (quickStart) quickStart.scrollIntoView({ behavior: 'smooth', block: 'start' });
      } catch (error) {
        status.textContent = t('failed') + (error && error.message ? error.message : String(error));
      } finally {
        minting = false;
        setMintLoading(false);
      }
    }
    function setMintLoading(loading) {
      mintButtons.forEach(button => {
        button.disabled = loading;
        button.classList.toggle('is-loading', loading);
        if (loading) {
          if (!button.dataset.label) button.dataset.label = button.textContent;
          button.innerHTML = '<span class="loading-spinner" aria-hidden="true"></span><span>' + escapeHTML(t('generating')) + '</span>';
        } else {
          const key = button.id === 'mint' ? 'generateSignal' : 'generate';
          button.textContent = t(key);
        }
      });
    }
    function signalErrorMessage(statusCode, text) {
      if (statusCode === 401) return t('invalidControlToken');
      if (statusCode === 403) return t('directTokenCannotGenerate');
      if (statusCode === 429) return t('rateLimited');
      return t('failed') + responseErrorDetail(text);
    }
    function responseErrorDetail(text) {
      if (!text) return '';
      try {
        const payload = JSON.parse(text);
        return payload.error || text;
      } catch {
        return text;
      }
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
      renderLatestVersion();
    }
    function t(key) {
      return dictionaries[currentLang][key] || dictionaries.en[key] || key;
    }
    function commandBlock(title, value, copyable) {
      return '<div class="command"><div class="command-title"><span>' + escapeHTML(title) + '</span>' +
        (copyable ? '<div class="command-actions"><button data-copy="' + escapeAttr(value) + '" onclick="copyValue(this)">' + escapeHTML(t('copy')) + '</button></div>' : '') +
        '</div><pre>' + escapeHTML(value) + '</pre></div>';
    }
    function linkBlock(title, value) {
      return '<div class="command"><div class="command-title"><span>' + escapeHTML(title) + '</span>' +
        '<div class="command-actions"><a href="' + escapeAttr(value) + '">' + escapeHTML(t('open')) + '</a>' +
        '<button data-copy="' + escapeAttr(value) + '" onclick="copyValue(this)">' + escapeHTML(t('copy')) + '</button></div>' +
        '</div><pre><a href="' + escapeAttr(value) + '">' + escapeHTML(value) + '</a></pre></div>';
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
    async function loadLatestVersion() {
      try {
        const response = await fetch(latestReleaseAPI, { headers: { 'Accept': 'application/vnd.github+json' } });
        if (!response.ok) throw new Error(String(response.status));
        latestRelease = await response.json();
        renderLatestVersion();
      } catch {
        const link = document.getElementById('version-link');
        link.textContent = 'latest';
        link.href = fallbackReleaseURL;
      }
    }
    function renderLatestVersion() {
      if (!latestRelease) return;
      const tag = latestRelease.tag_name || latestRelease.name || 'latest';
      const link = document.getElementById('version-link');
      link.textContent = tag;
      link.href = latestRelease.html_url || fallbackReleaseURL;
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
      setStatus('Session created', workerID + '/' + payload.name, 'ok');
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
