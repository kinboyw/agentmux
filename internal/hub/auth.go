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
	defaultSignalTTL          = 10 * time.Minute
	defaultCredentialTTL      = 24 * time.Hour
	defaultControlAccessTTL   = 15 * time.Minute
	defaultRefreshTokenTTL    = 7 * 24 * time.Hour
	defaultDeviceAuthTTL      = 10 * time.Minute
	devicePollIntervalSeconds = 2
	deviceSlowDownSeconds     = 5
	maxDeviceApproveFailures  = 8
	minDevicePollInterval     = devicePollIntervalSeconds * time.Second
)

type authStore struct {
	mu          sync.Mutex
	signals     map[string]signalEntry
	credentials map[string]credentialEntry
	refreshes   map[string]refreshTokenEntry
	users       map[string]userEntry
	devices     map[string]deviceAuthEntry
}

type AuthStore interface {
	MintSignal(ttl time.Duration, uses int, scopes []string) (mintedSignal, error)
	MintSignalForTenant(tenantID string, ttl time.Duration, uses int, scopes []string) (mintedSignal, error)
	Exchange(req exchangeRequest) (exchangedCredential, error)
	Register(req registerRequest) (authCredentialResponse, error)
	Login(req loginRequest) (authCredentialResponse, error)
	Refresh(req refreshRequest) (authCredentialResponse, error)
	StartDeviceAuth(req deviceStartRequest) (deviceStartResponse, error)
	ApproveDeviceAuth(req deviceApproveRequest) (authCredentialResponse, error)
	ApproveDeviceAuthForUser(userEmail, userCode string) (authCredentialResponse, error)
	PollDeviceAuth(req devicePollRequest) (devicePollResponse, error)
	DeviceAuthInfo(userCode string) (deviceAuthInfo, bool)
	Credential(token string) (credentialEntry, bool)
}

type signalEntry struct {
	Hash            string
	ID              string
	TenantID        string
	ExpiresAt       time.Time
	UsesRemaining   int
	Scopes          []string
	CreatedAt       time.Time
	UsedByInstances []string
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
	UserEmail string
	Role      string
	DeviceID  string
	Name      string
	Scopes    []string
	ExpiresAt time.Time
	CreatedAt time.Time
}

type refreshTokenEntry struct {
	Hash       string
	ID         string
	UserEmail  string
	TenantID   string
	Role       string
	DeviceID   string
	DeviceName string
	Status     string
	ExpiresAt  time.Time
	CreatedAt  time.Time
	LastUsedAt time.Time
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

type deviceAuthEntry struct {
	DeviceCodeHash string
	UserCodeHash   string
	UserCode       string
	DeviceID       string
	DeviceName     string
	Status         string
	UserEmail      string
	Credential     string
	RefreshToken   string
	AttemptCount   int
	ExpiresAt      time.Time
	CreatedAt      time.Time
	ApprovedAt     time.Time
	LastAttemptAt  time.Time
	LastPollAt     time.Time
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
	Credential       string       `json:"credential"`
	CredentialID     string       `json:"credential_id"`
	TenantID         string       `json:"tenant_id"`
	Role             string       `json:"role"`
	DeviceID         string       `json:"device_id"`
	ExpiresAt        time.Time    `json:"expires_at"`
	Scopes           []string     `json:"scopes"`
	RefreshToken     string       `json:"refresh_token,omitempty"`
	RefreshExpiresAt time.Time    `json:"refresh_expires_at,omitempty"`
	User             authUserView `json:"user"`
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
	InstanceID string `json:"instance_id,omitempty"`
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

type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

type deviceStartRequest struct {
	DeviceID   string `json:"device_id"`
	DeviceName string `json:"device_name"`
}

type deviceStartResponse struct {
	DeviceCode              string    `json:"device_code"`
	UserCode                string    `json:"user_code"`
	VerificationURL         string    `json:"verification_url"`
	VerificationURLComplete string    `json:"verification_url_complete"`
	ExpiresAt               time.Time `json:"expires_at"`
	Interval                int       `json:"interval"`
}

type deviceApproveRequest struct {
	UserCode string `json:"user_code"`
	Email    string `json:"email"`
	Password string `json:"password"`
}

type devicePollRequest struct {
	DeviceCode string `json:"device_code"`
}

type devicePollResponse struct {
	Status     string                  `json:"status"`
	Credential *authCredentialResponse `json:"credential,omitempty"`
	Interval   int                     `json:"interval,omitempty"`
	ExpiresAt  time.Time               `json:"expires_at,omitempty"`
}

type deviceAuthInfo struct {
	UserCode   string    `json:"user_code"`
	DeviceID   string    `json:"device_id"`
	DeviceName string    `json:"device_name"`
	Status     string    `json:"status"`
	ExpiresAt  time.Time `json:"expires_at"`
	CreatedAt  time.Time `json:"created_at"`
}

func newAuthStore() *authStore {
	return &authStore{
		signals:     map[string]signalEntry{},
		credentials: map[string]credentialEntry{},
		refreshes:   map[string]refreshTokenEntry{},
		users:       map[string]userEntry{},
		devices:     map[string]deviceAuthEntry{},
	}
}

func (s *authStore) MintSignal(ttl time.Duration, uses int, scopes []string) (mintedSignal, error) {
	return s.MintSignalForTenant("", ttl, uses, scopes)
}

func (s *authStore) MintSignalForTenant(tenantID string, ttl time.Duration, uses int, scopes []string) (mintedSignal, error) {
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
	instanceID := strings.TrimSpace(req.InstanceID)
	if instanceID != "" && slices.Contains(signal.UsedByInstances, instanceID) {
		return exchangedCredential{}, fmt.Errorf("signal has already been used by this worker instance")
	}
	if signal.UsesRemaining > 0 {
		signal.UsesRemaining--
	}
	if instanceID != "" {
		signal.UsedByInstances = append(signal.UsedByInstances, instanceID)
	}
	s.signals[hash] = signal
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
	return s.issueControlCredentialLocked(user, req.DeviceID, req.DeviceName, now)
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
	return s.issueControlCredentialLocked(user, req.DeviceID, req.DeviceName, now)
}

func (s *authStore) Refresh(req refreshRequest) (authCredentialResponse, error) {
	if req.RefreshToken == "" {
		return authCredentialResponse{}, fmt.Errorf("refresh token is required")
	}
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupLocked(now)
	refresh, ok := s.refreshes[tokenHash(req.RefreshToken)]
	if !ok || now.After(refresh.ExpiresAt) || refresh.Status != "active" {
		return authCredentialResponse{}, fmt.Errorf("invalid or expired refresh token")
	}
	user, ok := s.users[refresh.UserEmail]
	if !ok {
		return authCredentialResponse{}, fmt.Errorf("refresh user is missing")
	}
	delete(s.refreshes, refresh.Hash)
	return s.issueControlCredentialLocked(user, refresh.DeviceID, refresh.DeviceName, now)
}

func (s *authStore) StartDeviceAuth(req deviceStartRequest) (deviceStartResponse, error) {
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
	s.cleanupLocked(now)
	s.devices[entry.DeviceCodeHash] = entry
	s.mu.Unlock()
	return deviceStartResponse{
		DeviceCode: deviceCode, UserCode: userCode,
		ExpiresAt: entry.ExpiresAt, Interval: devicePollIntervalSeconds,
	}, nil
}

func (s *authStore) ApproveDeviceAuth(req deviceApproveRequest) (authCredentialResponse, error) {
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
	s.cleanupLocked(now)
	var deviceHash string
	var device deviceAuthEntry
	for hash, entry := range s.devices {
		if entry.UserCodeHash == tokenHash(code) {
			deviceHash = hash
			device = entry
			break
		}
	}
	if deviceHash == "" || now.After(device.ExpiresAt) {
		return authCredentialResponse{}, fmt.Errorf("authorization failed")
	}
	if device.Status != "pending" {
		return authCredentialResponse{}, fmt.Errorf("device authorization is %s", device.Status)
	}
	if device.AttemptCount >= maxDeviceApproveFailures {
		device.Status = "blocked"
		s.devices[deviceHash] = device
		return authCredentialResponse{}, fmt.Errorf("device authorization is blocked")
	}
	user, ok := s.users[email]
	if !ok || !passwordMatches(req.Password, user.PasswordSalt, user.PasswordHash) {
		device.AttemptCount++
		device.LastAttemptAt = now
		if device.AttemptCount >= maxDeviceApproveFailures {
			device.Status = "blocked"
		}
		s.devices[deviceHash] = device
		return authCredentialResponse{}, fmt.Errorf("authorization failed")
	}
	return s.approveDeviceAuthForUserLocked(deviceHash, device, user, now)
}

func (s *authStore) ApproveDeviceAuthForUser(userEmail, userCode string) (authCredentialResponse, error) {
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
	s.cleanupLocked(now)
	user, ok := s.users[email]
	if !ok {
		return authCredentialResponse{}, fmt.Errorf("authenticated user is missing")
	}
	var deviceHash string
	var device deviceAuthEntry
	for hash, entry := range s.devices {
		if entry.UserCodeHash == tokenHash(code) {
			deviceHash = hash
			device = entry
			break
		}
	}
	if deviceHash == "" || now.After(device.ExpiresAt) {
		return authCredentialResponse{}, fmt.Errorf("authorization failed")
	}
	if device.Status != "pending" {
		return authCredentialResponse{}, fmt.Errorf("device authorization is %s", device.Status)
	}
	if device.AttemptCount >= maxDeviceApproveFailures {
		device.Status = "blocked"
		s.devices[deviceHash] = device
		return authCredentialResponse{}, fmt.Errorf("device authorization is blocked")
	}
	return s.approveDeviceAuthForUserLocked(deviceHash, device, user, now)
}

func (s *authStore) approveDeviceAuthForUserLocked(deviceHash string, device deviceAuthEntry, user userEntry, now time.Time) (authCredentialResponse, error) {
	credential, err := s.issueControlCredentialLocked(user, device.DeviceID, device.DeviceName, now)
	if err != nil {
		return authCredentialResponse{}, err
	}
	device.Status = "approved"
	device.UserEmail = user.Email
	device.Credential = credential.Credential
	device.RefreshToken = credential.RefreshToken
	device.ApprovedAt = now
	s.devices[deviceHash] = device
	return credential, nil
}

func (s *authStore) PollDeviceAuth(req devicePollRequest) (devicePollResponse, error) {
	if req.DeviceCode == "" {
		return devicePollResponse{}, fmt.Errorf("device code is required")
	}
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupLocked(now)
	device, ok := s.devices[tokenHash(req.DeviceCode)]
	if !ok || now.After(device.ExpiresAt) {
		return devicePollResponse{Status: "expired"}, nil
	}
	if device.Status != "approved" {
		if device.Status == "pending" && !device.LastPollAt.IsZero() && now.Sub(device.LastPollAt) < minDevicePollInterval {
			return devicePollResponse{Status: "slow_down", Interval: deviceSlowDownSeconds, ExpiresAt: device.ExpiresAt}, nil
		}
		device.LastPollAt = now
		s.devices[tokenHash(req.DeviceCode)] = device
		return devicePollResponse{Status: device.Status, Interval: devicePollIntervalSeconds, ExpiresAt: device.ExpiresAt}, nil
	}
	credential, ok := s.credentials[tokenHash(device.Credential)]
	if !ok {
		return devicePollResponse{}, fmt.Errorf("approved credential is missing")
	}
	user, ok := s.users[device.UserEmail]
	if !ok {
		return devicePollResponse{}, fmt.Errorf("approved user is missing")
	}
	refresh, ok := s.refreshes[tokenHash(device.RefreshToken)]
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
	delete(s.devices, tokenHash(req.DeviceCode))
	return devicePollResponse{Status: "approved", Credential: &response}, nil
}

func (s *authStore) DeviceAuthInfo(userCode string) (deviceAuthInfo, bool) {
	code := normalizeUserCode(userCode)
	if code == "" {
		return deviceAuthInfo{}, false
	}
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupLocked(now)
	for _, entry := range s.devices {
		if entry.UserCodeHash == tokenHash(code) {
			return deviceInfoFromEntry(entry), true
		}
	}
	return deviceAuthInfo{}, false
}

func (s *authStore) issueControlCredentialLocked(user userEntry, deviceID, deviceName string, now time.Time) (authCredentialResponse, error) {
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
	s.credentials[entry.Hash] = entry
	refresh := refreshTokenEntry{
		Hash: tokenHash(refreshToken), ID: "ref_" + randomID(), UserEmail: user.Email,
		TenantID: user.TenantID, Role: "control", DeviceID: deviceID, DeviceName: deviceName,
		Status: "active", ExpiresAt: now.Add(defaultRefreshTokenTTL), CreatedAt: now,
	}
	s.refreshes[refresh.Hash] = refresh
	return authCredentialResponse{
		Credential: credential, CredentialID: entry.ID, TenantID: entry.TenantID,
		Role: entry.Role, DeviceID: entry.DeviceID, ExpiresAt: entry.ExpiresAt,
		Scopes: append([]string(nil), entry.Scopes...), RefreshToken: refreshToken,
		RefreshExpiresAt: refresh.ExpiresAt,
		User:             authUserView{ID: user.ID, TenantID: user.TenantID, Email: user.Email, Name: user.Name},
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
	for hash, entry := range s.refreshes {
		if now.After(entry.ExpiresAt) || entry.Status != "active" {
			delete(s.refreshes, hash)
		}
	}
	for hash, entry := range s.devices {
		if now.After(entry.ExpiresAt) {
			delete(s.devices, hash)
		}
	}
}

func deviceInfoFromEntry(entry deviceAuthEntry) deviceAuthInfo {
	deviceName := strings.TrimSpace(entry.DeviceName)
	if deviceName == "" {
		deviceName = entry.DeviceID
	}
	return deviceAuthInfo{
		UserCode: entry.UserCode, DeviceID: entry.DeviceID, DeviceName: deviceName,
		Status: entry.Status, ExpiresAt: entry.ExpiresAt, CreatedAt: entry.CreatedAt,
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

func randomUserCode() (string, error) {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	const codeLength = 16
	raw := make([]byte, codeLength)
	for i := range raw {
		for {
			var b [1]byte
			if _, err := rand.Read(b[:]); err != nil {
				return "", err
			}
			if int(b[0]) >= 256-(256%len(alphabet)) {
				continue
			}
			raw[i] = alphabet[int(b[0])%len(alphabet)]
			break
		}
	}
	out := make([]byte, 0, codeLength+3)
	for i, b := range raw {
		if i > 0 && i%4 == 0 {
			out = append(out, '-')
		}
		out = append(out, b)
	}
	return string(out), nil
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func installWorkerCommand(baseURL string, signal string) string {
	return fmt.Sprintf("curl -fsSL %s/install.sh | sh -s -- worker --join %s --name \"$(hostname)\"", baseURL, shellQuote(signal))
}

func workerJoinCommand(baseURL string, signal string) string {
	return fmt.Sprintf("agentmux worker join --hub %s --join %s --name \"$(hostname)\"", shellQuote(websocketBase(baseURL)), shellQuote(signal))
}

func installControlCommand(baseURL string) string {
	return fmt.Sprintf("agentmux-tui --hub %s", shellQuote(baseURL))
}

func installControlDirectCommand(baseURL string, token string) string {
	return fmt.Sprintf("agentmux-tui --hub %s --token %s", shellQuote(baseURL), shellQuote(token))
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

func normalizeUserCode(code string) string {
	code = strings.ToUpper(strings.TrimSpace(code))
	code = strings.ReplaceAll(code, "-", "")
	code = strings.ReplaceAll(code, " ", "")
	if code == "" {
		return ""
	}
	var builder strings.Builder
	for i, r := range code {
		if i > 0 && i%4 == 0 {
			builder.WriteByte('-')
		}
		builder.WriteRune(r)
	}
	return builder.String()
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
