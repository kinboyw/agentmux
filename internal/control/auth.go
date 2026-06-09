package control

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"private/agentmux/internal/credentialcache"
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

func ResolveAppAuth(ctx context.Context, opts AppAuthOptions) (AppAuthResult, error) {
	hubURL := credentialcache.NormalizeHubURL(opts.HubURL)
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
		entry := credentialcache.Entry{
			HubURL: hubURL, Credential: credential.Credential, CredentialID: credential.CredentialID,
			TenantID: credential.TenantID, Role: credential.Role, DeviceID: credential.DeviceID,
			DeviceName: deviceName, ExpiresAt: credential.ExpiresAt, UpdatedAt: time.Now().UTC(),
		}
		_ = credentialcache.Save(entry)
		return AppAuthResult{
			Client: New(hubURL, credential.Credential), CredentialID: credential.CredentialID,
			TenantID: credential.TenantID, DeviceID: credential.DeviceID, Role: credential.Role,
			ExpiresAt: credential.ExpiresAt, Source: "join",
		}, nil
	}
	if opts.Token != "" {
		return AppAuthResult{Client: New(hubURL, opts.Token), Source: "token"}, nil
	}
	var entry credentialcache.Entry
	var ok bool
	if hubURL == "" {
		entry, ok = credentialcache.LoadLatest("control", opts.DeviceID)
	} else {
		entry, ok = credentialcache.Load(hubURL, "control", opts.DeviceID)
	}
	if !ok {
		return AppAuthResult{}, errors.New("no credential available; pass --join or --token")
	}
	return AppAuthResult{
		Client: New(entry.HubURL, entry.Credential), CredentialID: entry.CredentialID,
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
