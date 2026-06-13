package hub

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"private/agentmux/internal/protocol"

	_ "modernc.org/sqlite"
)

type sqliteAuthStore struct {
	mu sync.Mutex
	db *sql.DB
}

func OpenSQLiteAuthStore(path string) (*sqliteAuthStore, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("sqlite path is required")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("prepare sqlite directory %s: %w", dir, err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite database %s: %w", path, err)
	}
	store := &sqliteAuthStore{db: db}
	if err := store.migrate(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initialize sqlite database %s: %w", path, err)
	}
	return store, nil
}

func (s *sqliteAuthStore) Close() error {
	return s.db.Close()
}

func (s *sqliteAuthStore) LoadWorkers() ([]workerRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(
		`SELECT worker_id, worker_instance_id, tenant_id, name, addr, backend, software_json, last_seen, connected, disabled, trace_enabled, debug_enabled FROM workers ORDER BY last_seen DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	workers := []workerRecord{}
	for rows.Next() {
		var worker workerRecord
		var softwareJSON, lastSeen string
		var connected, disabled, traceEnabled, debugEnabled int
		if err := rows.Scan(
			&worker.id, &worker.instanceID, &worker.tenantID, &worker.name, &worker.addr, &worker.backend, &softwareJSON, &lastSeen,
			&connected, &disabled, &traceEnabled, &debugEnabled,
		); err != nil {
			return nil, err
		}
		worker.software = unmarshalWorkerSoftware(softwareJSON)
		worker.lastSeen = parseStoredTime(lastSeen)
		worker.connected = intToBool(connected)
		worker.disabled = intToBool(disabled)
		worker.traceEnabled = intToBool(traceEnabled)
		worker.debugEnabled = intToBool(debugEnabled)
		workers = append(workers, worker)
	}
	return workers, rows.Err()
}

func (s *sqliteAuthStore) SaveWorker(worker workerRecord) error {
	if strings.TrimSpace(worker.id) == "" {
		return nil
	}
	softwareJSON, err := marshalWorkerSoftware(worker.software)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if worker.lastSeen.IsZero() {
		worker.lastSeen = now
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.db.Exec(
		`INSERT INTO workers(worker_id, worker_instance_id, tenant_id, name, addr, backend, software_json, last_seen, connected, disabled, trace_enabled, debug_enabled, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(worker_id) DO UPDATE SET
			worker_instance_id = excluded.worker_instance_id,
			tenant_id = excluded.tenant_id,
			name = excluded.name,
			addr = excluded.addr,
			backend = excluded.backend,
			software_json = excluded.software_json,
			last_seen = excluded.last_seen,
			connected = excluded.connected,
			disabled = excluded.disabled,
			trace_enabled = excluded.trace_enabled,
			debug_enabled = excluded.debug_enabled,
			updated_at = excluded.updated_at`,
		worker.id, worker.instanceID, worker.tenantID, worker.name, worker.addr, worker.backend, softwareJSON, formatTime(worker.lastSeen),
		boolToInt(worker.connected), boolToInt(worker.disabled), boolToInt(worker.traceEnabled), boolToInt(worker.debugEnabled), formatTime(now),
	)
	return err
}

func (s *sqliteAuthStore) DeleteWorker(workerID string) error {
	workerID = strings.TrimSpace(workerID)
	if workerID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.Exec(`DELETE FROM update_events WHERE worker_id = ?`, workerID); err != nil {
		return err
	}
	if _, err := s.db.Exec(`DELETE FROM update_jobs WHERE worker_id = ?`, workerID); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM workers WHERE worker_id = ?`, workerID)
	return err
}

func (s *sqliteAuthStore) DeleteTenantRuntime(tenantID string) error {
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.Exec(`DELETE FROM update_events WHERE tenant_id = ?`, tenantID); err != nil {
		return err
	}
	if _, err := s.db.Exec(`DELETE FROM update_jobs WHERE tenant_id = ?`, tenantID); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM workers WHERE tenant_id = ?`, tenantID)
	return err
}

func (s *sqliteAuthStore) LoadUpdateJobs() ([]workerUpdateJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(
		`SELECT id, tenant_id, worker_id, worker_instance_id, target_version, repo, status, message, allow_disruptive_restart, created_at, updated_at, finished_at FROM update_jobs ORDER BY updated_at DESC`,
	)
	if err != nil {
		return nil, err
	}
	jobs := []workerUpdateJob{}
	for rows.Next() {
		var job workerUpdateJob
		var createdAt, updatedAt, finishedAt string
		var allowDisruptiveRestart int
		if err := rows.Scan(
			&job.ID, &job.TenantID, &job.WorkerID, &job.WorkerInstanceID, &job.TargetVersion, &job.Repo, &job.Status, &job.Message,
			&allowDisruptiveRestart, &createdAt, &updatedAt, &finishedAt,
		); err != nil {
			return nil, err
		}
		job.AllowDisruptiveRestart = intToBool(allowDisruptiveRestart)
		job.CreatedAt = parseStoredTime(createdAt)
		job.UpdatedAt = parseStoredTime(updatedAt)
		job.FinishedAt = parseStoredTime(finishedAt)
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for index := range jobs {
		events, err := s.listUpdateEventsLocked(jobs[index].ID)
		if err != nil {
			return nil, err
		}
		jobs[index].Events = events
	}
	return jobs, nil
}

func (s *sqliteAuthStore) SaveUpdateJob(job workerUpdateJob) error {
	if strings.TrimSpace(job.ID) == "" {
		return nil
	}
	if job.CreatedAt.IsZero() {
		job.CreatedAt = time.Now().UTC()
	}
	if job.UpdatedAt.IsZero() {
		job.UpdatedAt = job.CreatedAt
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(
		`INSERT INTO update_jobs(id, tenant_id, worker_id, worker_instance_id, target_version, repo, status, message, allow_disruptive_restart, created_at, updated_at, finished_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
			tenant_id = excluded.tenant_id,
			worker_id = excluded.worker_id,
			worker_instance_id = excluded.worker_instance_id,
			target_version = excluded.target_version,
			repo = excluded.repo,
			status = excluded.status,
			message = excluded.message,
			allow_disruptive_restart = excluded.allow_disruptive_restart,
			updated_at = excluded.updated_at,
			finished_at = excluded.finished_at`,
		job.ID, job.TenantID, job.WorkerID, job.WorkerInstanceID, job.TargetVersion, job.Repo, job.Status, job.Message,
		boolToInt(job.AllowDisruptiveRestart), formatTime(job.CreatedAt), formatTime(job.UpdatedAt), formatTime(job.FinishedAt),
	)
	return err
}

func (s *sqliteAuthStore) AppendUpdateEvent(event workerUpdateEvent) error {
	if strings.TrimSpace(event.ID) == "" || strings.TrimSpace(event.JobID) == "" {
		return nil
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO update_events(id, job_id, tenant_id, worker_id, worker_instance_id, status, message, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		event.ID, event.JobID, event.TenantID, event.WorkerID, event.WorkerInstanceID, event.Status, event.Message, formatTime(event.CreatedAt),
	)
	return err
}

func (s *sqliteAuthStore) ListUpdateEvents(jobID string) ([]workerUpdateEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listUpdateEventsLocked(jobID)
}

func (s *sqliteAuthStore) listUpdateEventsLocked(jobID string) ([]workerUpdateEvent, error) {
	rows, err := s.db.Query(
		`SELECT id, job_id, tenant_id, worker_id, worker_instance_id, status, message, created_at FROM update_events WHERE job_id = ? ORDER BY created_at ASC`,
		jobID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := []workerUpdateEvent{}
	for rows.Next() {
		var event workerUpdateEvent
		var createdAt string
		if err := rows.Scan(&event.ID, &event.JobID, &event.TenantID, &event.WorkerID, &event.WorkerInstanceID, &event.Status, &event.Message, &createdAt); err != nil {
			return nil, err
		}
		event.CreatedAt = parseStoredTime(createdAt)
		events = append(events, event)
	}
	return events, rows.Err()
}

func (s *sqliteAuthStore) migrate() error {
	statements := []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA busy_timeout=5000`,
		`CREATE TABLE IF NOT EXISTS signals (
			hash TEXT PRIMARY KEY,
			id TEXT NOT NULL,
			tenant_id TEXT NOT NULL,
			expires_at TEXT NOT NULL,
			uses_remaining INTEGER NOT NULL,
			scopes_json TEXT NOT NULL,
			created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS signal_uses (
			signal_hash TEXT NOT NULL,
			instance_id TEXT NOT NULL,
			created_at TEXT NOT NULL,
			PRIMARY KEY(signal_hash, instance_id)
		)`,
		`CREATE TABLE IF NOT EXISTS credentials (
			hash TEXT PRIMARY KEY,
			id TEXT NOT NULL,
			tenant_id TEXT NOT NULL,
			user_email TEXT NOT NULL DEFAULT '',
			role TEXT NOT NULL,
			device_id TEXT NOT NULL,
			name TEXT NOT NULL,
			scopes_json TEXT NOT NULL,
			expires_at TEXT NOT NULL,
			created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS users (
			email TEXT PRIMARY KEY,
			id TEXT NOT NULL,
			tenant_id TEXT NOT NULL,
			name TEXT NOT NULL,
			password_salt TEXT NOT NULL,
			password_hash TEXT NOT NULL,
			created_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_credentials_tenant ON credentials(tenant_id)`,
		`CREATE TABLE IF NOT EXISTS refresh_tokens (
			hash TEXT PRIMARY KEY,
			id TEXT NOT NULL,
			user_email TEXT NOT NULL,
			tenant_id TEXT NOT NULL,
			role TEXT NOT NULL,
			device_id TEXT NOT NULL,
			device_name TEXT NOT NULL,
			status TEXT NOT NULL,
			expires_at TEXT NOT NULL,
			created_at TEXT NOT NULL,
			last_used_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_refresh_tokens_user_device ON refresh_tokens(user_email, device_id)`,
		`CREATE TABLE IF NOT EXISTS device_auth_sessions (
			device_code_hash TEXT PRIMARY KEY,
			user_code_hash TEXT NOT NULL,
			user_code TEXT NOT NULL,
			device_id TEXT NOT NULL,
			device_name TEXT NOT NULL,
			status TEXT NOT NULL,
			user_email TEXT NOT NULL,
			credential TEXT NOT NULL,
			refresh_token TEXT NOT NULL DEFAULT '',
			attempt_count INTEGER NOT NULL DEFAULT 0,
			expires_at TEXT NOT NULL,
			created_at TEXT NOT NULL,
			approved_at TEXT NOT NULL,
			last_attempt_at TEXT NOT NULL DEFAULT '',
			last_poll_at TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_device_auth_user_code ON device_auth_sessions(user_code_hash)`,
		`CREATE TABLE IF NOT EXISTS workers (
			worker_id TEXT PRIMARY KEY,
			worker_instance_id TEXT NOT NULL DEFAULT '',
			tenant_id TEXT NOT NULL,
			name TEXT NOT NULL,
			addr TEXT NOT NULL,
			backend TEXT NOT NULL,
			software_json TEXT NOT NULL,
			last_seen TEXT NOT NULL,
			connected INTEGER NOT NULL DEFAULT 0,
			disabled INTEGER NOT NULL DEFAULT 0,
			trace_enabled INTEGER NOT NULL DEFAULT 0,
			debug_enabled INTEGER NOT NULL DEFAULT 0,
			updated_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_workers_tenant ON workers(tenant_id)`,
		`CREATE INDEX IF NOT EXISTS idx_workers_instance ON workers(worker_instance_id)`,
		`CREATE TABLE IF NOT EXISTS update_jobs (
			id TEXT PRIMARY KEY,
			tenant_id TEXT NOT NULL,
			worker_id TEXT NOT NULL,
			worker_instance_id TEXT NOT NULL DEFAULT '',
			target_version TEXT NOT NULL,
			repo TEXT NOT NULL,
			status TEXT NOT NULL,
			message TEXT NOT NULL,
			allow_disruptive_restart INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			finished_at TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_update_jobs_worker ON update_jobs(worker_id, updated_at)`,
		`CREATE INDEX IF NOT EXISTS idx_update_jobs_tenant ON update_jobs(tenant_id, updated_at)`,
		`CREATE TABLE IF NOT EXISTS update_events (
			id TEXT PRIMARY KEY,
			job_id TEXT NOT NULL,
			tenant_id TEXT NOT NULL,
			worker_id TEXT NOT NULL,
			worker_instance_id TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL,
			message TEXT NOT NULL,
			created_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_update_events_job ON update_events(job_id, created_at)`,
	}
	for _, statement := range statements {
		if _, err := s.db.Exec(statement); err != nil {
			return err
		}
	}
	for _, statement := range []string{
		`ALTER TABLE credentials ADD COLUMN user_email TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE device_auth_sessions ADD COLUMN refresh_token TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE device_auth_sessions ADD COLUMN attempt_count INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE device_auth_sessions ADD COLUMN last_attempt_at TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE device_auth_sessions ADD COLUMN last_poll_at TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE workers ADD COLUMN worker_instance_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE update_jobs ADD COLUMN worker_instance_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE update_events ADD COLUMN worker_instance_id TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := s.db.Exec(statement); err != nil && !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
			return err
		}
	}
	return nil
}

func (s *sqliteAuthStore) MintSignal(ttl time.Duration, uses int, scopes []string) (mintedSignal, error) {
	return s.MintSignalForTenant("", ttl, uses, scopes)
}

func (s *sqliteAuthStore) MintSignalForTenant(tenantID string, ttl time.Duration, uses int, scopes []string) (mintedSignal, error) {
	if ttl <= 0 {
		ttl = defaultSignalTTL
	}
	if uses <= 0 {
		uses = -1
	}
	if len(scopes) == 0 {
		scopes = []string{"worker:join"}
	}
	token, err := randomToken("amx_sig_")
	if err != nil {
		return mintedSignal{}, err
	}
	now := time.Now().UTC()
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" {
		tenantID = "anon_" + randomID()
	}
	entry := signalEntry{
		Hash:          tokenHash(token),
		ID:            "sig_" + randomID(),
		TenantID:      tenantID,
		ExpiresAt:     now.Add(ttl),
		UsesRemaining: uses,
		Scopes:        append([]string(nil), scopes...),
		CreatedAt:     now,
	}
	scopesJSON, err := marshalScopes(entry.Scopes)
	if err != nil {
		return mintedSignal{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.cleanupLocked(now); err != nil {
		return mintedSignal{}, err
	}
	_, err = s.db.Exec(
		`INSERT INTO signals(hash, id, tenant_id, expires_at, uses_remaining, scopes_json, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		entry.Hash, entry.ID, entry.TenantID, formatTime(entry.ExpiresAt), entry.UsesRemaining, scopesJSON, formatTime(entry.CreatedAt),
	)
	if err != nil {
		return mintedSignal{}, err
	}
	return mintedSignal{
		Token: token, Signal: token, ID: entry.ID, TenantID: entry.TenantID,
		ExpiresAt: entry.ExpiresAt, UsesRemaining: entry.UsesRemaining,
		Scopes: append([]string(nil), entry.Scopes...),
	}, nil
}

func (s *sqliteAuthStore) Exchange(req exchangeRequest) (exchangedCredential, error) {
	role := strings.TrimSpace(req.Role)
	if role != "worker" && role != "control" {
		return exchangedCredential{}, fmt.Errorf("role must be worker or control")
	}
	scope := role + ":join"
	if req.Signal == "" {
		return exchangedCredential{}, fmt.Errorf("signal is required")
	}
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.cleanupLocked(now); err != nil {
		return exchangedCredential{}, err
	}
	signal, ok, err := s.signalByHash(tokenHash(req.Signal))
	if err != nil {
		return exchangedCredential{}, err
	}
	if !ok || now.After(signal.ExpiresAt) || signal.UsesRemaining == 0 {
		return exchangedCredential{}, fmt.Errorf("invalid or expired signal")
	}
	if !slices.Contains(signal.Scopes, scope) {
		return exchangedCredential{}, fmt.Errorf("signal scope does not allow %s", role)
	}
	instanceID := strings.TrimSpace(req.InstanceID)
	if instanceID != "" {
		var existing string
		err := s.db.QueryRow(`SELECT instance_id FROM signal_uses WHERE signal_hash = ? AND instance_id = ?`, signal.Hash, instanceID).Scan(&existing)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return exchangedCredential{}, err
		}
		if existing != "" {
			return exchangedCredential{}, fmt.Errorf("signal has already been used by this worker instance")
		}
	}
	if signal.UsesRemaining > 0 {
		if _, err := s.db.Exec(`UPDATE signals SET uses_remaining = ? WHERE hash = ?`, signal.UsesRemaining-1, signal.Hash); err != nil {
			return exchangedCredential{}, err
		}
	}
	if instanceID != "" {
		if _, err := s.db.Exec(`INSERT INTO signal_uses(signal_hash, instance_id, created_at) VALUES (?, ?, ?)`, signal.Hash, instanceID, formatTime(now)); err != nil {
			return exchangedCredential{}, err
		}
	}
	credential, err := randomToken("amx_cred_")
	if err != nil {
		return exchangedCredential{}, err
	}
	deviceID := strings.TrimSpace(req.DeviceID)
	if deviceID == "" {
		deviceID = "dev_" + randomID()
	}
	entry := credentialEntry{
		Hash:      tokenHash(credential),
		ID:        "cred_" + randomID(),
		TenantID:  signal.TenantID,
		Role:      role,
		DeviceID:  deviceID,
		Name:      strings.TrimSpace(req.DeviceName),
		Scopes:    credentialScopes(role),
		ExpiresAt: now.Add(defaultCredentialTTL),
		CreatedAt: now,
	}
	if err := s.insertCredential(entry); err != nil {
		return exchangedCredential{}, err
	}
	result := exchangedCredential{
		Credential: credential, CredentialID: entry.ID, TenantID: entry.TenantID,
		Role: entry.Role, DeviceID: entry.DeviceID, ExpiresAt: entry.ExpiresAt,
		Scopes: append([]string(nil), entry.Scopes...),
	}
	if role == "worker" {
		refreshToken, err := randomToken("amx_ref_")
		if err != nil {
			return exchangedCredential{}, err
		}
		refresh := refreshTokenEntry{
			Hash: tokenHash(refreshToken), ID: "ref_" + randomID(), TenantID: entry.TenantID,
			Role: role, DeviceID: entry.DeviceID, DeviceName: entry.Name,
			Status: "active", ExpiresAt: now.Add(defaultRefreshTokenTTL), CreatedAt: now,
		}
		if err := s.insertRefresh(refresh); err != nil {
			return exchangedCredential{}, err
		}
		result.RefreshToken = refreshToken
		result.RefreshExpiresAt = refresh.ExpiresAt
	}
	return result, nil
}

func (s *sqliteAuthStore) Register(req registerRequest) (authCredentialResponse, error) {
	email := normalizeEmail(req.Email)
	if email == "" {
		return authCredentialResponse{}, fmt.Errorf("email is required")
	}
	if len(req.Password) < 8 {
		return authCredentialResponse{}, fmt.Errorf("password must be at least 8 characters")
	}
	salt, hash, err := passwordDigest(req.Password)
	if err != nil {
		return authCredentialResponse{}, err
	}
	now := time.Now().UTC()
	user := userEntry{
		ID: "usr_" + randomID(), TenantID: "tenant_" + randomID(), Email: email,
		Name: strings.TrimSpace(req.Name), PasswordSalt: salt, PasswordHash: hash, CreatedAt: now,
	}
	if user.Name == "" {
		user.Name = email
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.cleanupLocked(now); err != nil {
		return authCredentialResponse{}, err
	}
	if _, ok, err := s.userByEmail(email); err != nil {
		return authCredentialResponse{}, err
	} else if ok {
		return authCredentialResponse{}, fmt.Errorf("user already exists")
	}
	_, err = s.db.Exec(
		`INSERT INTO users(email, id, tenant_id, name, password_salt, password_hash, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		user.Email, user.ID, user.TenantID, user.Name, user.PasswordSalt, user.PasswordHash, formatTime(user.CreatedAt),
	)
	if err != nil {
		return authCredentialResponse{}, err
	}
	return s.issueControlCredentialLocked(user, req.DeviceID, req.DeviceName, now)
}

func (s *sqliteAuthStore) Login(req loginRequest) (authCredentialResponse, error) {
	email := normalizeEmail(req.Email)
	if email == "" || req.Password == "" {
		return authCredentialResponse{}, fmt.Errorf("email and password are required")
	}
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.cleanupLocked(now); err != nil {
		return authCredentialResponse{}, err
	}
	user, ok, err := s.userByEmail(email)
	if err != nil {
		return authCredentialResponse{}, err
	}
	if !ok || !passwordMatches(req.Password, user.PasswordSalt, user.PasswordHash) {
		return authCredentialResponse{}, fmt.Errorf("invalid email or password")
	}
	return s.issueControlCredentialLocked(user, req.DeviceID, req.DeviceName, now)
}

func (s *sqliteAuthStore) Refresh(req refreshRequest) (authCredentialResponse, error) {
	if req.RefreshToken == "" {
		return authCredentialResponse{}, fmt.Errorf("refresh token is required")
	}
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.cleanupLocked(now); err != nil {
		return authCredentialResponse{}, err
	}
	refresh, ok, err := s.refreshByHash(tokenHash(req.RefreshToken))
	if err != nil {
		return authCredentialResponse{}, err
	}
	if !ok || now.After(refresh.ExpiresAt) || refresh.Status != "active" {
		return authCredentialResponse{}, fmt.Errorf("invalid or expired refresh token")
	}
	if _, err := s.db.Exec(`DELETE FROM refresh_tokens WHERE hash = ?`, refresh.Hash); err != nil {
		return authCredentialResponse{}, err
	}
	if refresh.Role == "worker" {
		return s.issueWorkerCredentialLocked(refresh.TenantID, refresh.DeviceID, refresh.DeviceName, now)
	}
	user, ok, err := s.userByEmail(refresh.UserEmail)
	if err != nil {
		return authCredentialResponse{}, err
	}
	if !ok {
		return authCredentialResponse{}, fmt.Errorf("refresh user is missing")
	}
	return s.issueControlCredentialLocked(user, refresh.DeviceID, refresh.DeviceName, now)
}

func (s *sqliteAuthStore) StartDeviceAuth(req deviceStartRequest) (deviceStartResponse, error) {
	deviceCode, err := randomToken("amx_dev_")
	if err != nil {
		return deviceStartResponse{}, err
	}
	userCode, err := randomUserCode()
	if err != nil {
		return deviceStartResponse{}, err
	}
	now := time.Now().UTC()
	deviceID := strings.TrimSpace(req.DeviceID)
	if deviceID == "" {
		deviceID = "dev_" + randomID()
	}
	entry := deviceAuthEntry{
		DeviceCodeHash: tokenHash(deviceCode),
		UserCodeHash:   tokenHash(normalizeUserCode(userCode)),
		UserCode:       userCode,
		DeviceID:       deviceID,
		DeviceName:     strings.TrimSpace(req.DeviceName),
		Status:         "pending",
		ExpiresAt:      now.Add(defaultDeviceAuthTTL),
		CreatedAt:      now,
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.cleanupLocked(now); err != nil {
		return deviceStartResponse{}, err
	}
	_, err = s.db.Exec(
		`INSERT INTO device_auth_sessions(device_code_hash, user_code_hash, user_code, device_id, device_name, status, user_email, credential, refresh_token, attempt_count, expires_at, created_at, approved_at, last_attempt_at, last_poll_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		entry.DeviceCodeHash, entry.UserCodeHash, entry.UserCode, entry.DeviceID, entry.DeviceName, entry.Status,
		entry.UserEmail, entry.Credential, entry.RefreshToken, entry.AttemptCount, formatTime(entry.ExpiresAt), formatTime(entry.CreatedAt),
		formatTime(entry.ApprovedAt), formatTime(entry.LastAttemptAt), formatTime(entry.LastPollAt),
	)
	if err != nil {
		return deviceStartResponse{}, err
	}
	return deviceStartResponse{DeviceCode: deviceCode, UserCode: userCode, ExpiresAt: entry.ExpiresAt, Interval: devicePollIntervalSeconds}, nil
}

func (s *sqliteAuthStore) ApproveDeviceAuth(req deviceApproveRequest) (authCredentialResponse, error) {
	code := normalizeUserCode(req.UserCode)
	if code == "" {
		return authCredentialResponse{}, fmt.Errorf("user code is required")
	}
	email := normalizeEmail(req.Email)
	if email == "" || req.Password == "" {
		return authCredentialResponse{}, fmt.Errorf("email and password are required")
	}
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.cleanupLocked(now); err != nil {
		return authCredentialResponse{}, err
	}
	device, ok, err := s.deviceByUserCodeHash(tokenHash(code))
	if err != nil {
		return authCredentialResponse{}, err
	}
	if !ok || now.After(device.ExpiresAt) {
		return authCredentialResponse{}, fmt.Errorf("authorization failed")
	}
	if device.Status != "pending" {
		return authCredentialResponse{}, fmt.Errorf("device authorization is %s", device.Status)
	}
	if device.AttemptCount >= maxDeviceApproveFailures {
		_, _ = s.db.Exec(`UPDATE device_auth_sessions SET status = ? WHERE device_code_hash = ?`, "blocked", device.DeviceCodeHash)
		return authCredentialResponse{}, fmt.Errorf("device authorization is blocked")
	}
	user, ok, err := s.userByEmail(email)
	if err != nil {
		return authCredentialResponse{}, err
	}
	if !ok || !passwordMatches(req.Password, user.PasswordSalt, user.PasswordHash) {
		status := "pending"
		if device.AttemptCount+1 >= maxDeviceApproveFailures {
			status = "blocked"
		}
		_, _ = s.db.Exec(
			`UPDATE device_auth_sessions SET attempt_count = attempt_count + 1, status = ?, last_attempt_at = ? WHERE device_code_hash = ?`,
			status, formatTime(now), device.DeviceCodeHash,
		)
		return authCredentialResponse{}, fmt.Errorf("authorization failed")
	}
	return s.approveDeviceAuthForUserLocked(device, user, now)
}

func (s *sqliteAuthStore) ApproveDeviceAuthForUser(userEmail, userCode string) (authCredentialResponse, error) {
	code := normalizeUserCode(userCode)
	if code == "" {
		return authCredentialResponse{}, fmt.Errorf("user code is required")
	}
	email := normalizeEmail(userEmail)
	if email == "" {
		return authCredentialResponse{}, fmt.Errorf("authenticated user is required")
	}
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.cleanupLocked(now); err != nil {
		return authCredentialResponse{}, err
	}
	user, ok, err := s.userByEmail(email)
	if err != nil {
		return authCredentialResponse{}, err
	}
	if !ok {
		return authCredentialResponse{}, fmt.Errorf("authenticated user is missing")
	}
	device, ok, err := s.deviceByUserCodeHash(tokenHash(code))
	if err != nil {
		return authCredentialResponse{}, err
	}
	if !ok || now.After(device.ExpiresAt) {
		return authCredentialResponse{}, fmt.Errorf("authorization failed")
	}
	if device.Status != "pending" {
		return authCredentialResponse{}, fmt.Errorf("device authorization is %s", device.Status)
	}
	if device.AttemptCount >= maxDeviceApproveFailures {
		_, _ = s.db.Exec(`UPDATE device_auth_sessions SET status = ? WHERE device_code_hash = ?`, "blocked", device.DeviceCodeHash)
		return authCredentialResponse{}, fmt.Errorf("device authorization is blocked")
	}
	return s.approveDeviceAuthForUserLocked(device, user, now)
}

func (s *sqliteAuthStore) approveDeviceAuthForUserLocked(device deviceAuthEntry, user userEntry, now time.Time) (authCredentialResponse, error) {
	credential, err := s.issueControlCredentialLocked(user, device.DeviceID, device.DeviceName, now)
	if err != nil {
		return authCredentialResponse{}, err
	}
	_, err = s.db.Exec(
		`UPDATE device_auth_sessions SET status = ?, user_email = ?, credential = ?, refresh_token = ?, approved_at = ? WHERE device_code_hash = ?`,
		"approved", user.Email, credential.Credential, credential.RefreshToken, formatTime(now), device.DeviceCodeHash,
	)
	if err != nil {
		return authCredentialResponse{}, err
	}
	return credential, nil
}

func (s *sqliteAuthStore) PollDeviceAuth(req devicePollRequest) (devicePollResponse, error) {
	if req.DeviceCode == "" {
		return devicePollResponse{}, fmt.Errorf("device code is required")
	}
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.cleanupLocked(now); err != nil {
		return devicePollResponse{}, err
	}
	device, ok, err := s.deviceByDeviceCodeHash(tokenHash(req.DeviceCode))
	if err != nil {
		return devicePollResponse{}, err
	}
	if !ok || now.After(device.ExpiresAt) {
		return devicePollResponse{Status: "expired"}, nil
	}
	if device.Status != "approved" {
		if device.Status == "pending" && !device.LastPollAt.IsZero() && now.Sub(device.LastPollAt) < minDevicePollInterval {
			return devicePollResponse{Status: "slow_down", Interval: deviceSlowDownSeconds, ExpiresAt: device.ExpiresAt}, nil
		}
		_, _ = s.db.Exec(`UPDATE device_auth_sessions SET last_poll_at = ? WHERE device_code_hash = ?`, formatTime(now), device.DeviceCodeHash)
		return devicePollResponse{Status: device.Status, Interval: devicePollIntervalSeconds, ExpiresAt: device.ExpiresAt}, nil
	}
	credential, ok, err := s.credentialByHash(tokenHash(device.Credential))
	if err != nil {
		return devicePollResponse{}, err
	}
	if !ok {
		return devicePollResponse{}, fmt.Errorf("approved credential is missing")
	}
	user, ok, err := s.userByEmail(device.UserEmail)
	if err != nil {
		return devicePollResponse{}, err
	}
	if !ok {
		return devicePollResponse{}, fmt.Errorf("approved user is missing")
	}
	refresh, ok, err := s.refreshByHash(tokenHash(device.RefreshToken))
	if err != nil {
		return devicePollResponse{}, err
	}
	if !ok {
		return devicePollResponse{}, fmt.Errorf("approved refresh token is missing")
	}
	response := authCredentialResponse{
		Credential: device.Credential, CredentialID: credential.ID, TenantID: credential.TenantID,
		Role: credential.Role, DeviceID: credential.DeviceID, ExpiresAt: credential.ExpiresAt,
		Scopes: append([]string(nil), credential.Scopes...), RefreshToken: device.RefreshToken,
		RefreshExpiresAt: refresh.ExpiresAt,
		User:             authUserView{ID: user.ID, TenantID: user.TenantID, Email: user.Email, Name: user.Name},
	}
	_, _ = s.db.Exec(`DELETE FROM device_auth_sessions WHERE device_code_hash = ?`, device.DeviceCodeHash)
	return devicePollResponse{Status: "approved", Credential: &response}, nil
}

func (s *sqliteAuthStore) DeviceAuthInfo(userCode string) (deviceAuthInfo, bool) {
	code := normalizeUserCode(userCode)
	if code == "" {
		return deviceAuthInfo{}, false
	}
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.cleanupLocked(now); err != nil {
		return deviceAuthInfo{}, false
	}
	device, ok, err := s.deviceByUserCodeHash(tokenHash(code))
	if err != nil || !ok || now.After(device.ExpiresAt) {
		return deviceAuthInfo{}, false
	}
	return deviceInfoFromEntry(device), true
}

func (s *sqliteAuthStore) Credential(token string) (credentialEntry, bool) {
	if token == "" {
		return credentialEntry{}, false
	}
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.cleanupLocked(now); err != nil {
		return credentialEntry{}, false
	}
	entry, ok, err := s.credentialByHash(tokenHash(token))
	if err != nil || !ok || now.After(entry.ExpiresAt) {
		return credentialEntry{}, false
	}
	return entry, true
}

func (s *sqliteAuthStore) HasActiveControlCredential(tenantID string, now time.Time) bool {
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" {
		return false
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.cleanupLocked(now); err != nil {
		return false
	}
	var count int
	err := s.db.QueryRow(
		`SELECT COUNT(1) FROM credentials WHERE tenant_id = ? AND role = ? AND expires_at > ?`,
		tenantID, "control", formatTime(now),
	).Scan(&count)
	return err == nil && count > 0
}

func (s *sqliteAuthStore) issueControlCredentialLocked(user userEntry, deviceID, deviceName string, now time.Time) (authCredentialResponse, error) {
	credential, err := randomToken("amx_cred_")
	if err != nil {
		return authCredentialResponse{}, err
	}
	refreshToken, err := randomToken("amx_ref_")
	if err != nil {
		return authCredentialResponse{}, err
	}
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		deviceID = "dev_" + randomID()
	}
	deviceName = strings.TrimSpace(deviceName)
	entry := credentialEntry{
		Hash: tokenHash(credential), ID: "cred_" + randomID(), TenantID: user.TenantID,
		UserEmail: user.Email, Role: "control", DeviceID: deviceID, Name: deviceName,
		Scopes: credentialScopes("control"), ExpiresAt: now.Add(defaultControlAccessTTL), CreatedAt: now,
	}
	if err := s.insertCredential(entry); err != nil {
		return authCredentialResponse{}, err
	}
	refresh := refreshTokenEntry{
		Hash: tokenHash(refreshToken), ID: "ref_" + randomID(), UserEmail: user.Email,
		TenantID: user.TenantID, Role: "control", DeviceID: deviceID, DeviceName: deviceName,
		Status: "active", ExpiresAt: now.Add(defaultRefreshTokenTTL), CreatedAt: now,
	}
	if err := s.insertRefresh(refresh); err != nil {
		return authCredentialResponse{}, err
	}
	return authCredentialResponse{
		Credential: credential, CredentialID: entry.ID, TenantID: entry.TenantID,
		Role: entry.Role, DeviceID: entry.DeviceID, ExpiresAt: entry.ExpiresAt,
		Scopes: append([]string(nil), entry.Scopes...), RefreshToken: refreshToken,
		RefreshExpiresAt: refresh.ExpiresAt,
		User:             authUserView{ID: user.ID, TenantID: user.TenantID, Email: user.Email, Name: user.Name},
	}, nil
}

func (s *sqliteAuthStore) issueWorkerCredentialLocked(tenantID, deviceID, deviceName string, now time.Time) (authCredentialResponse, error) {
	credential, err := randomToken("amx_cred_")
	if err != nil {
		return authCredentialResponse{}, err
	}
	refreshToken, err := randomToken("amx_ref_")
	if err != nil {
		return authCredentialResponse{}, err
	}
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		deviceID = "dev_" + randomID()
	}
	deviceName = strings.TrimSpace(deviceName)
	entry := credentialEntry{
		Hash: tokenHash(credential), ID: "cred_" + randomID(), TenantID: tenantID,
		Role: "worker", DeviceID: deviceID, Name: deviceName,
		Scopes: credentialScopes("worker"), ExpiresAt: now.Add(defaultCredentialTTL), CreatedAt: now,
	}
	if err := s.insertCredential(entry); err != nil {
		return authCredentialResponse{}, err
	}
	refresh := refreshTokenEntry{
		Hash: tokenHash(refreshToken), ID: "ref_" + randomID(), TenantID: tenantID,
		Role: "worker", DeviceID: deviceID, DeviceName: deviceName,
		Status: "active", ExpiresAt: now.Add(defaultRefreshTokenTTL), CreatedAt: now,
	}
	if err := s.insertRefresh(refresh); err != nil {
		return authCredentialResponse{}, err
	}
	return authCredentialResponse{
		Credential: credential, CredentialID: entry.ID, TenantID: entry.TenantID,
		Role: entry.Role, DeviceID: entry.DeviceID, ExpiresAt: entry.ExpiresAt,
		Scopes: append([]string(nil), entry.Scopes...), RefreshToken: refreshToken,
		RefreshExpiresAt: refresh.ExpiresAt,
	}, nil
}

func (s *sqliteAuthStore) insertCredential(entry credentialEntry) error {
	scopesJSON, err := marshalScopes(entry.Scopes)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
		`INSERT INTO credentials(hash, id, tenant_id, user_email, role, device_id, name, scopes_json, expires_at, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		entry.Hash, entry.ID, entry.TenantID, entry.UserEmail, entry.Role, entry.DeviceID, entry.Name, scopesJSON, formatTime(entry.ExpiresAt), formatTime(entry.CreatedAt),
	)
	return err
}

func (s *sqliteAuthStore) insertRefresh(entry refreshTokenEntry) error {
	_, err := s.db.Exec(
		`INSERT INTO refresh_tokens(hash, id, user_email, tenant_id, role, device_id, device_name, status, expires_at, created_at, last_used_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		entry.Hash, entry.ID, entry.UserEmail, entry.TenantID, entry.Role, entry.DeviceID, entry.DeviceName, entry.Status,
		formatTime(entry.ExpiresAt), formatTime(entry.CreatedAt), formatTime(entry.LastUsedAt),
	)
	return err
}

func (s *sqliteAuthStore) cleanupLocked(now time.Time) error {
	_, err := s.db.Exec(`DELETE FROM signals WHERE expires_at < ? OR uses_remaining = 0`, formatTime(now))
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`DELETE FROM signal_uses WHERE signal_hash NOT IN (SELECT hash FROM signals)`)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`DELETE FROM credentials WHERE expires_at < ?`, formatTime(now))
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`DELETE FROM refresh_tokens WHERE expires_at < ? OR status != ?`, formatTime(now), "active")
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`DELETE FROM device_auth_sessions WHERE expires_at < ?`, formatTime(now))
	return err
}

func (s *sqliteAuthStore) signalByHash(hash string) (signalEntry, bool, error) {
	var entry signalEntry
	var expiresAt, createdAt, scopesJSON string
	err := s.db.QueryRow(
		`SELECT hash, id, tenant_id, expires_at, uses_remaining, scopes_json, created_at FROM signals WHERE hash = ?`, hash,
	).Scan(&entry.Hash, &entry.ID, &entry.TenantID, &expiresAt, &entry.UsesRemaining, &scopesJSON, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return signalEntry{}, false, nil
	}
	if err != nil {
		return signalEntry{}, false, err
	}
	entry.ExpiresAt = parseStoredTime(expiresAt)
	entry.CreatedAt = parseStoredTime(createdAt)
	entry.Scopes = unmarshalScopes(scopesJSON)
	return entry, true, nil
}

func (s *sqliteAuthStore) credentialByHash(hash string) (credentialEntry, bool, error) {
	var entry credentialEntry
	var expiresAt, createdAt, scopesJSON string
	err := s.db.QueryRow(
		`SELECT hash, id, tenant_id, user_email, role, device_id, name, scopes_json, expires_at, created_at FROM credentials WHERE hash = ?`, hash,
	).Scan(&entry.Hash, &entry.ID, &entry.TenantID, &entry.UserEmail, &entry.Role, &entry.DeviceID, &entry.Name, &scopesJSON, &expiresAt, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return credentialEntry{}, false, nil
	}
	if err != nil {
		return credentialEntry{}, false, err
	}
	entry.ExpiresAt = parseStoredTime(expiresAt)
	entry.CreatedAt = parseStoredTime(createdAt)
	entry.Scopes = unmarshalScopes(scopesJSON)
	return entry, true, nil
}

func (s *sqliteAuthStore) refreshByHash(hash string) (refreshTokenEntry, bool, error) {
	var entry refreshTokenEntry
	var expiresAt, createdAt, lastUsedAt string
	err := s.db.QueryRow(
		`SELECT hash, id, user_email, tenant_id, role, device_id, device_name, status, expires_at, created_at, last_used_at FROM refresh_tokens WHERE hash = ?`, hash,
	).Scan(
		&entry.Hash, &entry.ID, &entry.UserEmail, &entry.TenantID, &entry.Role, &entry.DeviceID,
		&entry.DeviceName, &entry.Status, &expiresAt, &createdAt, &lastUsedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return refreshTokenEntry{}, false, nil
	}
	if err != nil {
		return refreshTokenEntry{}, false, err
	}
	entry.ExpiresAt = parseStoredTime(expiresAt)
	entry.CreatedAt = parseStoredTime(createdAt)
	entry.LastUsedAt = parseStoredTime(lastUsedAt)
	return entry, true, nil
}

func (s *sqliteAuthStore) userByEmail(email string) (userEntry, bool, error) {
	var user userEntry
	var createdAt string
	err := s.db.QueryRow(
		`SELECT id, tenant_id, email, name, password_salt, password_hash, created_at FROM users WHERE email = ?`, email,
	).Scan(&user.ID, &user.TenantID, &user.Email, &user.Name, &user.PasswordSalt, &user.PasswordHash, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return userEntry{}, false, nil
	}
	if err != nil {
		return userEntry{}, false, err
	}
	user.CreatedAt = parseStoredTime(createdAt)
	return user, true, nil
}

func (s *sqliteAuthStore) deviceByUserCodeHash(hash string) (deviceAuthEntry, bool, error) {
	return s.deviceByColumn("user_code_hash", hash)
}

func (s *sqliteAuthStore) deviceByDeviceCodeHash(hash string) (deviceAuthEntry, bool, error) {
	return s.deviceByColumn("device_code_hash", hash)
}

func (s *sqliteAuthStore) deviceByColumn(column string, value string) (deviceAuthEntry, bool, error) {
	var entry deviceAuthEntry
	var expiresAt, createdAt, approvedAt, lastAttemptAt, lastPollAt string
	err := s.db.QueryRow(
		`SELECT device_code_hash, user_code_hash, user_code, device_id, device_name, status, user_email, credential, refresh_token, attempt_count, expires_at, created_at, approved_at, last_attempt_at, last_poll_at FROM device_auth_sessions WHERE `+column+` = ?`,
		value,
	).Scan(
		&entry.DeviceCodeHash, &entry.UserCodeHash, &entry.UserCode, &entry.DeviceID, &entry.DeviceName,
		&entry.Status, &entry.UserEmail, &entry.Credential, &entry.RefreshToken, &entry.AttemptCount,
		&expiresAt, &createdAt, &approvedAt, &lastAttemptAt, &lastPollAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return deviceAuthEntry{}, false, nil
	}
	if err != nil {
		return deviceAuthEntry{}, false, err
	}
	entry.ExpiresAt = parseStoredTime(expiresAt)
	entry.CreatedAt = parseStoredTime(createdAt)
	entry.ApprovedAt = parseStoredTime(approvedAt)
	entry.LastAttemptAt = parseStoredTime(lastAttemptAt)
	entry.LastPollAt = parseStoredTime(lastPollAt)
	return entry, true, nil
}

func marshalScopes(scopes []string) (string, error) {
	raw, err := json.Marshal(scopes)
	return string(raw), err
}

func unmarshalScopes(value string) []string {
	var scopes []string
	if err := json.Unmarshal([]byte(value), &scopes); err != nil {
		return nil
	}
	return scopes
}

func formatTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func parseStoredTime(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

func marshalWorkerSoftware(software protocol.WorkerSoftware) (string, error) {
	raw, err := json.Marshal(software)
	return string(raw), err
}

func unmarshalWorkerSoftware(value string) protocol.WorkerSoftware {
	var software protocol.WorkerSoftware
	if err := json.Unmarshal([]byte(value), &software); err != nil {
		return protocol.WorkerSoftware{}
	}
	return software
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func intToBool(value int) bool {
	return value != 0
}
