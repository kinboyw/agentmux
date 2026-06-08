package hub

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

const defaultJoinTokenTTL = 10 * time.Minute

type joinTokenStore struct {
	mu     sync.Mutex
	tokens map[string]joinToken
}

type joinToken struct {
	Hash          string    `json:"-"`
	ExpiresAt     time.Time `json:"expires_at"`
	UsesRemaining int       `json:"uses_remaining"`
	Reusable      bool      `json:"reusable"`
	Scopes        []string  `json:"scopes"`
}

type mintedJoinToken struct {
	Token         string    `json:"token"`
	ExpiresAt     time.Time `json:"expires_at"`
	UsesRemaining int       `json:"uses_remaining"`
	Scopes        []string  `json:"scopes"`
}

func newJoinTokenStore() *joinTokenStore {
	return &joinTokenStore{tokens: map[string]joinToken{}}
}

func (s *joinTokenStore) Mint(ttl time.Duration, uses int, scopes []string) (mintedJoinToken, error) {
	if ttl <= 0 {
		ttl = defaultJoinTokenTTL
	}
	if uses <= 0 {
		uses = 2
	}
	if len(scopes) == 0 {
		scopes = []string{"worker:join", "control:join"}
	}
	token, err := randomJoinToken()
	if err != nil {
		return mintedJoinToken{}, err
	}
	expiresAt := time.Now().UTC().Add(ttl)
	entry := joinToken{
		Hash:          tokenHash(token),
		ExpiresAt:     expiresAt,
		UsesRemaining: uses,
		Reusable:      true,
		Scopes:        append([]string(nil), scopes...),
	}
	s.mu.Lock()
	s.cleanupLocked(time.Now().UTC())
	s.tokens[entry.Hash] = entry
	s.mu.Unlock()
	return mintedJoinToken{
		Token: token, ExpiresAt: expiresAt, UsesRemaining: uses,
		Scopes: append([]string(nil), scopes...),
	}, nil
}

func (s *joinTokenStore) Valid(token string) bool {
	if token == "" {
		return false
	}
	now := time.Now().UTC()
	hash := tokenHash(token)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupLocked(now)
	entry, ok := s.tokens[hash]
	if !ok || now.After(entry.ExpiresAt) || (!entry.Reusable && entry.UsesRemaining <= 0) {
		return false
	}
	if entry.Reusable {
		return true
	}
	entry.UsesRemaining--
	if entry.UsesRemaining <= 0 {
		delete(s.tokens, hash)
	} else {
		s.tokens[hash] = entry
	}
	return true
}

func (s *joinTokenStore) cleanupLocked(now time.Time) {
	for hash, entry := range s.tokens {
		if now.After(entry.ExpiresAt) || entry.UsesRemaining <= 0 {
			delete(s.tokens, hash)
		}
	}
}

func randomJoinToken() (string, error) {
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "amx_join_" + base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func installWorkerCommand(baseURL string, token string) string {
	return fmt.Sprintf("go run ./cmd/agentmux worker --hub %s --join %s --name $(hostname)", websocketBase(baseURL), shellQuote(token))
}

func installControlCommand(baseURL string, token string) string {
	return fmt.Sprintf("go run ./cmd/agentmux control list --hub %s --join %s", baseURL, shellQuote(token))
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
