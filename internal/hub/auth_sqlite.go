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
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	store := &sqliteAuthStore{db: db}
	if err := store.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *sqliteAuthStore) Close() error {
	return s.db.Close()
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
		`CREATE TABLE IF NOT EXISTS credentials (
			hash TEXT PRIMARY KEY,
			id TEXT NOT NULL,
			tenant_id TEXT NOT NULL,
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
	}
	for _, statement := range statements {
		if _, err := s.db.Exec(statement); err != nil {
			return err
		}
	}
	return nil
}

func (s *sqliteAuthStore) MintSignal(ttl time.Duration, uses int, scopes []string) (mintedSignal, error) {
	if ttl <= 0 {
		ttl = defaultSignalTTL
	}
	if uses <= 0 {
		uses = -1
	}
	if len(scopes) == 0 {
		scopes = []string{"worker:join", "control:join"}
	}
	token, err := randomToken("amx_sig_")
	if err != nil {
		return mintedSignal{}, err
	}
	now := time.Now().UTC()
	entry := signalEntry{
		Hash:          tokenHash(token),
		ID:            "sig_" + randomID(),
		TenantID:      "anon_" + randomID(),
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
	if signal.UsesRemaining > 0 {
		if _, err := s.db.Exec(`UPDATE signals SET uses_remaining = ? WHERE hash = ?`, signal.UsesRemaining-1, signal.Hash); err != nil {
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
	return exchangedCredential{
		Credential: credential, CredentialID: entry.ID, TenantID: entry.TenantID,
		Role: entry.Role, DeviceID: entry.DeviceID, ExpiresAt: entry.ExpiresAt,
		Scopes: append([]string(nil), entry.Scopes...),
	}, nil
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
	return s.issueCredentialLocked(user, "control", req.DeviceID, req.DeviceName, now)
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
	return s.issueCredentialLocked(user, "control", req.DeviceID, req.DeviceName, now)
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

func (s *sqliteAuthStore) issueCredentialLocked(user userEntry, role, deviceID, deviceName string, now time.Time) (authCredentialResponse, error) {
	credential, err := randomToken("amx_cred_")
	if err != nil {
		return authCredentialResponse{}, err
	}
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		deviceID = "dev_" + randomID()
	}
	entry := credentialEntry{
		Hash: tokenHash(credential), ID: "cred_" + randomID(), TenantID: user.TenantID,
		Role: role, DeviceID: deviceID, Name: strings.TrimSpace(deviceName),
		Scopes: credentialScopes(role), ExpiresAt: now.Add(defaultCredentialTTL), CreatedAt: now,
	}
	if err := s.insertCredential(entry); err != nil {
		return authCredentialResponse{}, err
	}
	return authCredentialResponse{
		Credential: credential, CredentialID: entry.ID, TenantID: entry.TenantID,
		Role: entry.Role, DeviceID: entry.DeviceID, ExpiresAt: entry.ExpiresAt,
		Scopes: append([]string(nil), entry.Scopes...),
		User:   authUserView{ID: user.ID, TenantID: user.TenantID, Email: user.Email, Name: user.Name},
	}, nil
}

func (s *sqliteAuthStore) insertCredential(entry credentialEntry) error {
	scopesJSON, err := marshalScopes(entry.Scopes)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
		`INSERT INTO credentials(hash, id, tenant_id, role, device_id, name, scopes_json, expires_at, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		entry.Hash, entry.ID, entry.TenantID, entry.Role, entry.DeviceID, entry.Name, scopesJSON, formatTime(entry.ExpiresAt), formatTime(entry.CreatedAt),
	)
	return err
}

func (s *sqliteAuthStore) cleanupLocked(now time.Time) error {
	_, err := s.db.Exec(`DELETE FROM signals WHERE expires_at < ? OR uses_remaining = 0`, formatTime(now))
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`DELETE FROM credentials WHERE expires_at < ?`, formatTime(now))
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
		`SELECT hash, id, tenant_id, role, device_id, name, scopes_json, expires_at, created_at FROM credentials WHERE hash = ?`, hash,
	).Scan(&entry.Hash, &entry.ID, &entry.TenantID, &entry.Role, &entry.DeviceID, &entry.Name, &scopesJSON, &expiresAt, &createdAt)
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
