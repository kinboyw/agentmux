package hub

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	defaultSignalTTL     = 10 * time.Minute
	defaultCredentialTTL = 24 * time.Hour
)

type authStore struct {
	mu          sync.Mutex
	signals     map[string]signalEntry
	credentials map[string]credentialEntry
	users       map[string]userEntry
}

type signalEntry struct {
	Hash          string
	ID            string
	TenantID      string
	ExpiresAt     time.Time
	UsesRemaining int
	Scopes        []string
	CreatedAt     time.Time
}

type mintedSignal struct {
	Token         string    `json:"token"`
	Signal        string    `json:"signal"`
	ID            string    `json:"signal_id"`
	TenantID      string    `json:"tenant_id"`
	ExpiresAt     time.Time `json:"expires_at"`
	UsesRemaining int       `json:"uses_remaining"`
	Scopes        []string  `json:"scopes"`
}

type credentialEntry struct {
	Hash      string
	ID        string
	TenantID  string
	Role      string
	DeviceID  string
	Name      string
	Scopes    []string
	ExpiresAt time.Time
	CreatedAt time.Time
}

type userEntry struct {
	ID           string
	TenantID     string
	Email        string
	Name         string
	PasswordSalt string
	PasswordHash string
	CreatedAt    time.Time
}

type exchangedCredential struct {
	Credential   string    `json:"credential"`
	CredentialID string    `json:"credential_id"`
	TenantID     string    `json:"tenant_id"`
	Role         string    `json:"role"`
	DeviceID     string    `json:"device_id"`
	ExpiresAt    time.Time `json:"expires_at"`
	Scopes       []string  `json:"scopes"`
}

type authCredentialResponse struct {
	Credential   string       `json:"credential"`
	CredentialID string       `json:"credential_id"`
	TenantID     string       `json:"tenant_id"`
	Role         string       `json:"role"`
	DeviceID     string       `json:"device_id"`
	ExpiresAt    time.Time    `json:"expires_at"`
	Scopes       []string     `json:"scopes"`
	User         authUserView `json:"user"`
}

type authUserView struct {
	ID       string `json:"id"`
	TenantID string `json:"tenant_id"`
	Email    string `json:"email"`
	Name     string `json:"name"`
}

type exchangeRequest struct {
	Signal     string `json:"signal"`
	Role       string `json:"role"`
	DeviceID   string `json:"device_id"`
	DeviceName string `json:"device_name"`
}

type registerRequest struct {
	Email      string `json:"email"`
	Password   string `json:"password"`
	Name       string `json:"name"`
	DeviceID   string `json:"device_id"`
	DeviceName string `json:"device_name"`
}

type loginRequest struct {
	Email      string `json:"email"`
	Password   string `json:"password"`
	DeviceID   string `json:"device_id"`
	DeviceName string `json:"device_name"`
}

func newAuthStore() *authStore {
	return &authStore{
		signals:     map[string]signalEntry{},
		credentials: map[string]credentialEntry{},
		users:       map[string]userEntry{},
	}
}

func (s *authStore) MintSignal(ttl time.Duration, uses int, scopes []string) (mintedSignal, error) {
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
	s.mu.Lock()
	s.cleanupLocked(now)
	s.signals[entry.Hash] = entry
	s.mu.Unlock()
	return mintedSignal{
		Token: token, Signal: token, ID: entry.ID, TenantID: entry.TenantID,
		ExpiresAt: entry.ExpiresAt, UsesRemaining: entry.UsesRemaining,
		Scopes: append([]string(nil), entry.Scopes...),
	}, nil
}

func (s *authStore) Exchange(req exchangeRequest) (exchangedCredential, error) {
	role := strings.TrimSpace(req.Role)
	if role != "worker" && role != "control" {
		return exchangedCredential{}, fmt.Errorf("role must be worker or control")
	}
	scope := role + ":join"
	if req.Signal == "" {
		return exchangedCredential{}, fmt.Errorf("signal is required")
	}
	now := time.Now().UTC()
	hash := tokenHash(req.Signal)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupLocked(now)
	signal, ok := s.signals[hash]
	if !ok || now.After(signal.ExpiresAt) || signal.UsesRemaining == 0 {
		return exchangedCredential{}, fmt.Errorf("invalid or expired signal")
	}
	if !slices.Contains(signal.Scopes, scope) {
		return exchangedCredential{}, fmt.Errorf("signal scope does not allow %s", role)
	}
	if signal.UsesRemaining > 0 {
		signal.UsesRemaining--
		s.signals[hash] = signal
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
	s.credentials[entry.Hash] = entry
	return exchangedCredential{
		Credential: credential, CredentialID: entry.ID, TenantID: entry.TenantID,
		Role: entry.Role, DeviceID: entry.DeviceID, ExpiresAt: entry.ExpiresAt,
		Scopes: append([]string(nil), entry.Scopes...),
	}, nil
}

func (s *authStore) Register(req registerRequest) (authCredentialResponse, error) {
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
	s.cleanupLocked(now)
	if _, exists := s.users[email]; exists {
		return authCredentialResponse{}, fmt.Errorf("user already exists")
	}
	s.users[email] = user
	return s.issueCredentialLocked(user, "control", req.DeviceID, req.DeviceName, now)
}

func (s *authStore) Login(req loginRequest) (authCredentialResponse, error) {
	email := normalizeEmail(req.Email)
	if email == "" || req.Password == "" {
		return authCredentialResponse{}, fmt.Errorf("email and password are required")
	}
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupLocked(now)
	user, ok := s.users[email]
	if !ok || !passwordMatches(req.Password, user.PasswordSalt, user.PasswordHash) {
		return authCredentialResponse{}, fmt.Errorf("invalid email or password")
	}
	return s.issueCredentialLocked(user, "control", req.DeviceID, req.DeviceName, now)
}

func (s *authStore) issueCredentialLocked(user userEntry, role, deviceID, deviceName string, now time.Time) (authCredentialResponse, error) {
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
	s.credentials[entry.Hash] = entry
	return authCredentialResponse{
		Credential: credential, CredentialID: entry.ID, TenantID: entry.TenantID,
		Role: entry.Role, DeviceID: entry.DeviceID, ExpiresAt: entry.ExpiresAt,
		Scopes: append([]string(nil), entry.Scopes...),
		User:   authUserView{ID: user.ID, TenantID: user.TenantID, Email: user.Email, Name: user.Name},
	}, nil
}

func (s *authStore) Credential(token string) (credentialEntry, bool) {
	if token == "" {
		return credentialEntry{}, false
	}
	now := time.Now().UTC()
	hash := tokenHash(token)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupLocked(now)
	entry, ok := s.credentials[hash]
	if !ok || now.After(entry.ExpiresAt) {
		return credentialEntry{}, false
	}
	return entry, true
}

func (s *authStore) cleanupLocked(now time.Time) {
	for hash, entry := range s.signals {
		if now.After(entry.ExpiresAt) || entry.UsesRemaining == 0 {
			delete(s.signals, hash)
		}
	}
	for hash, entry := range s.credentials {
		if now.After(entry.ExpiresAt) {
			delete(s.credentials, hash)
		}
	}
}

func credentialScopes(role string) []string {
	if role == "worker" {
		return []string{"worker:connect", "session:report", "terminal:stream"}
	}
	return []string{"control:connect", "worker:list", "session:list", "session:create", "session:attach", "session:input", "session:stop"}
}

func randomToken(prefix string) (string, error) {
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func randomID() string {
	var raw [9]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "unknown"
	}
	return base64.RawURLEncoding.EncodeToString(raw[:])
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func installWorkerCommand(baseURL string, signal string) string {
	return fmt.Sprintf("go run ./cmd/agentmux worker --hub %s --join %s --name $(hostname)", websocketBase(baseURL), shellQuote(signal))
}

func installControlCommand(baseURL string, signal string) string {
	return fmt.Sprintf("go run ./cmd/agentmux control list --hub %s --join %s", baseURL, shellQuote(signal))
}

func websocketBase(baseURL string) string {
	if len(baseURL) >= 8 && baseURL[:8] == "https://" {
		return "wss://" + baseURL[8:]
	}
	if len(baseURL) >= 7 && baseURL[:7] == "http://" {
		return "ws://" + baseURL[7:]
	}
	return baseURL
}

func shellQuote(value string) string {
	return "'" + value + "'"
}

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func passwordDigest(password string) (string, string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", "", err
	}
	salt := base64.RawURLEncoding.EncodeToString(raw[:])
	return salt, passwordHash(password, salt), nil
}

func passwordHash(password, salt string) string {
	sum := sha256.Sum256([]byte(salt + "\x00" + password))
	return hex.EncodeToString(sum[:])
}

func passwordMatches(password, salt, expected string) bool {
	actual := passwordHash(password, salt)
	return subtle.ConstantTimeCompare([]byte(actual), []byte(expected)) == 1
}
