package control

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"private/agentmux/internal/protocol"
)

func (c *Client) ListWorkers(ctx context.Context, out io.Writer) error {
	workers, err := c.Workers(ctx)
	if err != nil {
		return err
	}
	for _, worker := range workers {
		fmt.Fprintf(out, "%s\t%s\t%s\t%s\n", worker.ID, worker.Name, worker.Addr, worker.LastSeen.Format("2006-01-02T15:04:05Z07:00"))
	}
	return nil
}

func (c *Client) ListSessions(ctx context.Context, out io.Writer) error {
	sessions, err := c.Sessions(ctx)
	if err != nil {
		return err
	}
	for _, session := range sessions {
		fmt.Fprintf(out, "%s\t%s\t%s\t%s\n", session.ID, session.Status, session.Command, session.CWD)
	}
	return nil
}

func (c *Client) Workers(ctx context.Context) ([]protocol.WorkerView, error) {
	var payload struct {
		Workers []protocol.WorkerView `json:"workers"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/api/workers", nil, &payload); err != nil {
		return nil, err
	}
	return payload.Workers, nil
}

func (c *Client) Sessions(ctx context.Context) ([]protocol.SessionView, error) {
	var payload struct {
		Sessions []protocol.SessionView `json:"sessions"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/api/sessions", nil, &payload); err != nil {
		return nil, err
	}
	return payload.Sessions, nil
}

func (c *Client) CreateSession(ctx context.Context, req protocol.CreateSession) error {
	var payload map[string]string
	return c.doJSON(ctx, http.MethodPost, "/api/sessions", req, &payload)
}

func (c *Client) SendInput(ctx context.Context, sessionID, data string) error {
	workerID, name, ok := protocol.SplitSessionID(sessionID)
	if !ok {
		return fmt.Errorf("invalid session id %q; expected worker/name", sessionID)
	}
	path := fmt.Sprintf("/api/sessions/%s/%s/input", url.PathEscape(workerID), url.PathEscape(name))
	var payload map[string]string
	return c.doJSON(ctx, http.MethodPost, path, protocol.TerminalInput{Data: data}, &payload)
}

func (c *Client) StopSession(ctx context.Context, sessionID string) error {
	workerID, name, ok := protocol.SplitSessionID(sessionID)
	if !ok {
		return fmt.Errorf("invalid session id %q; expected worker/name", sessionID)
	}
	path := fmt.Sprintf("/api/sessions/%s/%s", url.PathEscape(workerID), url.PathEscape(name))
	var payload map[string]string
	return c.doJSON(ctx, http.MethodDelete, path, nil, &payload)
}

func (c *Client) SessionPreview(ctx context.Context, sessionID string, lines int) (string, error) {
	workerID, name, ok := protocol.SplitSessionID(sessionID)
	if !ok {
		return "", fmt.Errorf("invalid session id %q; expected worker/name", sessionID)
	}
	if lines <= 0 {
		lines = 80
	}
	path := fmt.Sprintf("/api/sessions/%s/%s/preview?lines=%d", url.PathEscape(workerID), url.PathEscape(name), lines)
	var payload protocol.SessionPreview
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &payload); err != nil {
		return "", err
	}
	return payload.Data, nil
}

func (c *Client) ExchangeSignal(ctx context.Context, signal, role, deviceID, deviceName string) (string, error) {
	var payload struct {
		Credential string `json:"credential"`
		DeviceID   string `json:"device_id"`
	}
	req := map[string]string{
		"signal":      signal,
		"role":        role,
		"device_id":   deviceID,
		"device_name": deviceName,
	}
	client := *c
	client.Token = ""
	if err := client.doJSON(ctx, http.MethodPost, "/api/exchange", req, &payload); err != nil {
		return "", err
	}
	return payload.Credential, nil
}

func (c *Client) doJSON(ctx context.Context, method, path string, body any, target any) error {
	if c == nil {
		return fmt.Errorf("control client is nil")
	}
	if !skipAutoRefresh(path) {
		if err := c.EnsureFresh(ctx); err != nil {
			return err
		}
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.HubURL+path, reader)
	if err != nil {
		return err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s %s failed: %s: %s", method, path, resp.Status, strings.TrimSpace(string(data)))
	}
	if target == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(target)
}

func skipAutoRefresh(path string) bool {
	return path == "/api/exchange" ||
		path == "/api/auth/refresh" ||
		strings.HasPrefix(path, "/api/auth/device/")
}
