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
	Login      bool
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

type DeviceStartResponse struct {
	DeviceCode              string    `json:"device_code"`
	UserCode                string    `json:"user_code"`
	VerificationURL         string    `json:"verification_url"`
	VerificationURLComplete string    `json:"verification_url_complete"`
	ExpiresAt               time.Time `json:"expires_at"`
	Interval                int       `json:"interval"`
}

type DevicePollResponse struct {
	Status     string                         `json:"status"`
	Credential *authCredentialResponsePayload `json:"credential"`
	Interval   int                            `json:"interval"`
	ExpiresAt  time.Time                      `json:"expires_at"`
}

type authCredentialResponsePayload struct {
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
	if !ok && opts.Login {
		return DeviceLogin(ctx, hubURL, opts.DeviceID, deviceName, nil)
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

func DeviceLogin(ctx context.Context, hubURL, deviceID, deviceName string, prompt func(DeviceStartResponse)) (AppAuthResult, error) {
	hubURL = credentialcache.NormalizeHubURL(hubURL)
	if strings.TrimSpace(deviceName) == "" {
		deviceName = "control"
	}
	client := New(hubURL, "")
	start, err := client.StartDeviceAuth(ctx, deviceID, deviceName)
	if err != nil {
		return AppAuthResult{}, err
	}
	if prompt != nil {
		prompt(start)
	}
	interval := time.Duration(start.Interval) * time.Second
	if interval <= 0 {
		interval = 2 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return AppAuthResult{}, ctx.Err()
		case <-ticker.C:
			poll, err := client.PollDeviceAuth(ctx, start.DeviceCode)
			if err != nil {
				return AppAuthResult{}, err
			}
			switch poll.Status {
			case "pending":
				continue
			case "expired":
				return AppAuthResult{}, errors.New("device login expired")
			case "approved":
				if poll.Credential == nil {
					return AppAuthResult{}, errors.New("device login approved without credential")
				}
				credential := *poll.Credential
				entry := credentialcache.Entry{
					HubURL: hubURL, Credential: credential.Credential, CredentialID: credential.CredentialID,
					TenantID: credential.TenantID, Role: credential.Role, DeviceID: credential.DeviceID,
					DeviceName: deviceName, ExpiresAt: credential.ExpiresAt, UpdatedAt: time.Now().UTC(),
				}
				_ = credentialcache.Save(entry)
				return AppAuthResult{
					Client: New(hubURL, credential.Credential), CredentialID: credential.CredentialID,
					TenantID: credential.TenantID, DeviceID: credential.DeviceID, Role: credential.Role,
					ExpiresAt: credential.ExpiresAt, Source: "login",
				}, nil
			default:
				return AppAuthResult{}, errors.New("device login status: " + poll.Status)
			}
		}
	}
}

func (c Client) StartDeviceAuth(ctx context.Context, deviceID, deviceName string) (DeviceStartResponse, error) {
	req := map[string]string{"device_id": deviceID, "device_name": deviceName}
	var payload DeviceStartResponse
	if err := c.doJSON(ctx, http.MethodPost, "/api/auth/device/start", req, &payload); err != nil {
		return DeviceStartResponse{}, err
	}
	return payload, nil
}

func (c Client) PollDeviceAuth(ctx context.Context, deviceCode string) (DevicePollResponse, error) {
	req := map[string]string{"device_code": deviceCode}
	var payload DevicePollResponse
	if err := c.doJSON(ctx, http.MethodPost, "/api/auth/device/poll", req, &payload); err != nil {
		return DevicePollResponse{}, err
	}
	return payload, nil
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
