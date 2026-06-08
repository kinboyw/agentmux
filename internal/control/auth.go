package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type AppAuthOptions struct {
	HubURL     string
	Token      string
	Join       string
	DeviceID   string
	DeviceName string
}

type AppAuthResult struct {
	Client       Client
	CredentialID string
	TenantID     string
	DeviceID     string
	Role         string
	ExpiresAt    time.Time
	Source       string
}

type exchangedCredentialPayload struct {
	Credential   string    `json:"credential"`
	CredentialID string    `json:"credential_id"`
	TenantID     string    `json:"tenant_id"`
	Role         string    `json:"role"`
	DeviceID     string    `json:"device_id"`
	ExpiresAt    time.Time `json:"expires_at"`
	Scopes       []string  `json:"scopes"`
}

type credentialCache struct {
	Entries []credentialCacheEntry `json:"entries"`
}

type credentialCacheEntry struct {
	HubURL       string    `json:"hub_url"`
	Credential   string    `json:"credential"`
	CredentialID string    `json:"credential_id"`
	TenantID     string    `json:"tenant_id"`
	Role         string    `json:"role"`
	DeviceID     string    `json:"device_id"`
	DeviceName   string    `json:"device_name"`
	ExpiresAt    time.Time `json:"expires_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func ResolveAppAuth(ctx context.Context, opts AppAuthOptions) (AppAuthResult, error) {
	hubURL := strings.TrimRight(opts.HubURL, "/")
	deviceName := strings.TrimSpace(opts.DeviceName)
	if deviceName == "" {
		deviceName = "control"
	}
	if opts.Join != "" {
		client := New(hubURL, "")
		credential, err := client.ExchangeSignalDetail(ctx, opts.Join, "control", opts.DeviceID, deviceName)
		if err != nil {
			return AppAuthResult{}, err
		}
		entry := credentialCacheEntry{
			HubURL: hubURL, Credential: credential.Credential, CredentialID: credential.CredentialID,
			TenantID: credential.TenantID, Role: credential.Role, DeviceID: credential.DeviceID,
			DeviceName: deviceName, ExpiresAt: credential.ExpiresAt, UpdatedAt: time.Now().UTC(),
		}
		_ = saveCredentialCacheEntry(entry)
		return AppAuthResult{
			Client: New(hubURL, credential.Credential), CredentialID: credential.CredentialID,
			TenantID: credential.TenantID, DeviceID: credential.DeviceID, Role: credential.Role,
			ExpiresAt: credential.ExpiresAt, Source: "join",
		}, nil
	}
	if opts.Token != "" {
		return AppAuthResult{Client: New(hubURL, opts.Token), Source: "token"}, nil
	}
	entry, ok := loadCredentialCacheEntry(hubURL)
	if !ok {
		return AppAuthResult{}, errors.New("no credential available; pass --join or --token")
	}
	return AppAuthResult{
		Client: New(hubURL, entry.Credential), CredentialID: entry.CredentialID,
		TenantID: entry.TenantID, DeviceID: entry.DeviceID, Role: entry.Role,
		ExpiresAt: entry.ExpiresAt, Source: "cache",
	}, nil
}

func (c Client) ExchangeSignalDetail(ctx context.Context, signal, role, deviceID, deviceName string) (exchangedCredentialPayload, error) {
	req := map[string]string{
		"signal":      signal,
		"role":        role,
		"device_id":   deviceID,
		"device_name": deviceName,
	}
	client := c
	client.Token = ""
	var payload exchangedCredentialPayload
	if err := client.doJSON(ctx, http.MethodPost, "/api/exchange", req, &payload); err != nil {
		return exchangedCredentialPayload{}, err
	}
	return payload, nil
}

func credentialCachePath() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "agentmux", "credentials.json"), nil
}

func loadCredentialCacheEntry(hubURL string) (credentialCacheEntry, bool) {
	path, err := credentialCachePath()
	if err != nil {
		return credentialCacheEntry{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return credentialCacheEntry{}, false
	}
	var cache credentialCache
	if err := json.Unmarshal(data, &cache); err != nil {
		return credentialCacheEntry{}, false
	}
	now := time.Now().UTC()
	var newest credentialCacheEntry
	for _, entry := range cache.Entries {
		if entry.HubURL != hubURL || entry.Role != "control" || entry.Credential == "" {
			continue
		}
		if !entry.ExpiresAt.IsZero() && now.After(entry.ExpiresAt) {
			continue
		}
		if newest.Credential == "" || entry.UpdatedAt.After(newest.UpdatedAt) {
			newest = entry
		}
	}
	return newest, newest.Credential != ""
}

func saveCredentialCacheEntry(entry credentialCacheEntry) error {
	path, err := credentialCachePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	cache := credentialCache{}
	data, err := os.ReadFile(path)
	if err == nil {
		_ = json.Unmarshal(data, &cache)
	}
	next := cache.Entries[:0]
	for _, existing := range cache.Entries {
		if existing.HubURL == entry.HubURL && existing.Role == entry.Role && existing.DeviceID == entry.DeviceID {
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
