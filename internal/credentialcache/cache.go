package credentialcache

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Cache struct {
	Entries []Entry `json:"entries"`
}

type Entry struct {
	HubURL           string    `json:"hub_url"`
	Credential       string    `json:"credential"`
	CredentialID     string    `json:"credential_id"`
	TenantID         string    `json:"tenant_id"`
	Role             string    `json:"role"`
	DeviceID         string    `json:"device_id"`
	DeviceName       string    `json:"device_name"`
	ExpiresAt        time.Time `json:"expires_at"`
	RefreshToken     string    `json:"refresh_token,omitempty"`
	RefreshExpiresAt time.Time `json:"refresh_expires_at,omitempty"`
	UpdatedAt        time.Time `json:"updated_at"`
}

func Path() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "agentmux", "credentials.json"), nil
}

func NormalizeHubURL(hubURL string) string {
	value := strings.TrimRight(strings.TrimSpace(hubURL), "/")
	if strings.HasPrefix(value, "ws://") {
		value = "http://" + strings.TrimPrefix(value, "ws://")
	}
	if strings.HasPrefix(value, "wss://") {
		value = "https://" + strings.TrimPrefix(value, "wss://")
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return value
	}
	parsed.Path = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return strings.TrimRight(parsed.String(), "/")
}

func Load(hubURL, role, deviceID string) (Entry, bool) {
	return load(hubURL, role, deviceID, true)
}

func LoadLatest(role, deviceID string) (Entry, bool) {
	return load("", role, deviceID, false)
}

func load(hubURL, role, deviceID string, matchHub bool) (Entry, bool) {
	path, err := Path()
	if err != nil {
		return Entry{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Entry{}, false
	}
	var cache Cache
	if err := json.Unmarshal(data, &cache); err != nil {
		return Entry{}, false
	}
	hubURL = NormalizeHubURL(hubURL)
	role = strings.TrimSpace(role)
	deviceID = strings.TrimSpace(deviceID)
	now := time.Now().UTC()
	var newest Entry
	for _, entry := range cache.Entries {
		if matchHub && NormalizeHubURL(entry.HubURL) != hubURL {
			continue
		}
		if role != "" && entry.Role != role {
			continue
		}
		if entry.Credential == "" && entry.RefreshToken == "" {
			continue
		}
		if deviceID != "" && entry.DeviceID != deviceID {
			continue
		}
		accessExpired := entry.Credential == "" || (!entry.ExpiresAt.IsZero() && now.After(entry.ExpiresAt))
		refreshExpired := entry.RefreshToken == "" || (!entry.RefreshExpiresAt.IsZero() && now.After(entry.RefreshExpiresAt))
		if accessExpired && refreshExpired {
			continue
		}
		if newest.Credential == "" || entry.UpdatedAt.After(newest.UpdatedAt) {
			newest = entry
		}
	}
	return newest, newest.Credential != "" || newest.RefreshToken != ""
}

func Save(entry Entry) error {
	path, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	entry.HubURL = NormalizeHubURL(entry.HubURL)
	entry.Role = strings.TrimSpace(entry.Role)
	entry.DeviceID = strings.TrimSpace(entry.DeviceID)
	if entry.UpdatedAt.IsZero() {
		entry.UpdatedAt = time.Now().UTC()
	}
	cache := Cache{}
	data, err := os.ReadFile(path)
	if err == nil {
		_ = json.Unmarshal(data, &cache)
	}
	next := cache.Entries[:0]
	for _, existing := range cache.Entries {
		if NormalizeHubURL(existing.HubURL) == entry.HubURL && existing.Role == entry.Role && existing.DeviceID == entry.DeviceID {
			continue
		}
		next = append(next, existing)
	}
	cache.Entries = append(next, entry)
	raw, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return fmt.Errorf("write credential cache: %w", err)
	}
	return nil
}

func Delete(hubURL, role, deviceID string) error {
	path, err := Path()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var cache Cache
	if err := json.Unmarshal(data, &cache); err != nil {
		return err
	}
	hubURL = NormalizeHubURL(hubURL)
	role = strings.TrimSpace(role)
	deviceID = strings.TrimSpace(deviceID)
	next := cache.Entries[:0]
	for _, existing := range cache.Entries {
		if hubURL != "" && NormalizeHubURL(existing.HubURL) != hubURL {
			next = append(next, existing)
			continue
		}
		if role != "" && existing.Role != role {
			next = append(next, existing)
			continue
		}
		if deviceID != "" && existing.DeviceID != deviceID {
			next = append(next, existing)
			continue
		}
	}
	cache.Entries = next
	raw, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}
