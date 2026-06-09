package control

import (
	"context"
	"net/http"
	"strings"
	"time"

	"private/agentmux/internal/credentialcache"
)

type Client struct {
	HubURL           string
	Token            string
	CredentialID     string
	TenantID         string
	Role             string
	DeviceID         string
	DeviceName       string
	RefreshToken     string
	ExpiresAt        time.Time
	RefreshExpiresAt time.Time
	HTTP             *http.Client
}

func New(hubURL, token string) Client {
	return Client{HubURL: strings.TrimRight(hubURL, "/"), Token: token, HTTP: http.DefaultClient}
}

func (c Client) WithRefresh(refreshToken string, expiresAt time.Time, refreshExpiresAt time.Time) Client {
	c.RefreshToken = refreshToken
	c.ExpiresAt = expiresAt
	c.RefreshExpiresAt = refreshExpiresAt
	return c
}

func (c Client) WithCacheEntry(entry credentialcache.Entry) Client {
	c.CredentialID = entry.CredentialID
	c.TenantID = entry.TenantID
	c.Role = entry.Role
	c.DeviceID = entry.DeviceID
	c.DeviceName = entry.DeviceName
	c.RefreshToken = entry.RefreshToken
	c.ExpiresAt = entry.ExpiresAt
	c.RefreshExpiresAt = entry.RefreshExpiresAt
	return c
}

func (c *Client) EnsureFresh(ctx context.Context) error {
	if c == nil || c.RefreshToken == "" {
		return nil
	}
	if !c.RefreshExpiresAt.IsZero() && time.Now().UTC().After(c.RefreshExpiresAt) {
		return nil
	}
	if !c.ExpiresAt.IsZero() && time.Until(c.ExpiresAt) > 2*time.Minute {
		return nil
	}
	payload, err := (*c).RefreshCredential(ctx, c.RefreshToken)
	if err != nil {
		return err
	}
	c.Token = payload.Credential
	c.CredentialID = payload.CredentialID
	c.TenantID = payload.TenantID
	c.Role = payload.Role
	c.DeviceID = payload.DeviceID
	c.RefreshToken = payload.RefreshToken
	c.ExpiresAt = payload.ExpiresAt
	c.RefreshExpiresAt = payload.RefreshExpiresAt
	if c.DeviceName == "" {
		c.DeviceName = payload.User.Name
	}
	_ = credentialcache.Save(credentialcache.Entry{
		HubURL: c.HubURL, Credential: c.Token, CredentialID: c.CredentialID,
		TenantID: c.TenantID, Role: c.Role, DeviceID: c.DeviceID, DeviceName: c.DeviceName,
		ExpiresAt: c.ExpiresAt, RefreshToken: c.RefreshToken, RefreshExpiresAt: c.RefreshExpiresAt,
		UpdatedAt: time.Now().UTC(),
	})
	return nil
}
