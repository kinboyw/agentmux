package control

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"private/agentmux/internal/protocol"
	"private/agentmux/internal/term"
	"private/agentmux/internal/ws"
)

type Client struct {
	HubURL string
	Token  string
	HTTP   *http.Client
}

func New(hubURL, token string) Client {
	return Client{HubURL: strings.TrimRight(hubURL, "/"), Token: token, HTTP: http.DefaultClient}
}

func (c Client) ListWorkers(ctx context.Context, out io.Writer) error {
	var payload struct {
		Workers []protocol.WorkerView `json:"workers"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/api/workers", nil, &payload); err != nil {
		return err
	}
	for _, worker := range payload.Workers {
		fmt.Fprintf(out, "%s\t%s\t%s\t%s\n", worker.ID, worker.Name, worker.Addr, worker.LastSeen.Format("2006-01-02T15:04:05Z07:00"))
	}
	return nil
}

func (c Client) ListSessions(ctx context.Context, out io.Writer) error {
	var payload struct {
		Sessions []protocol.SessionView `json:"sessions"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/api/sessions", nil, &payload); err != nil {
		return err
	}
	for _, session := range payload.Sessions {
		fmt.Fprintf(out, "%s\t%s\t%s\t%s\n", session.ID, session.Status, session.Command, session.CWD)
	}
	return nil
}

func (c Client) CreateSession(ctx context.Context, req protocol.CreateSession) error {
	var payload map[string]string
	return c.doJSON(ctx, http.MethodPost, "/api/sessions", req, &payload)
}

func (c Client) SendInput(ctx context.Context, sessionID, data string) error {
	workerID, name, ok := protocol.SplitSessionID(sessionID)
	if !ok {
		return fmt.Errorf("invalid session id %q; expected worker/name", sessionID)
	}
	path := fmt.Sprintf("/api/sessions/%s/%s/input", url.PathEscape(workerID), url.PathEscape(name))
	var payload map[string]string
	return c.doJSON(ctx, http.MethodPost, path, protocol.TerminalInput{Data: data}, &payload)
}

func (c Client) StopSession(ctx context.Context, sessionID string) error {
	workerID, name, ok := protocol.SplitSessionID(sessionID)
	if !ok {
		return fmt.Errorf("invalid session id %q; expected worker/name", sessionID)
	}
	path := fmt.Sprintf("/api/sessions/%s/%s", url.PathEscape(workerID), url.PathEscape(name))
	var payload map[string]string
	return c.doJSON(ctx, http.MethodDelete, path, nil, &payload)
}

func (c Client) Attach(ctx context.Context, sessionID string, in io.Reader, out io.Writer) error {
	target, err := controlURL(c.HubURL, c.Token)
	if err != nil {
		return err
	}
	conn, err := ws.Dial(ctx, target, c.Token)
	if err != nil {
		return err
	}
	defer conn.Close()

	streamID := newStreamID(sessionID)
	cols, rows := terminalSize(in)
	open, err := protocol.NewEnvelope(protocol.TypeControlOpen, protocol.TerminalSize{Cols: cols, Rows: rows})
	if err != nil {
		return err
	}
	open.SessionID = sessionID
	open.StreamID = streamID
	if err := writeEnvelope(conn, open); err != nil {
		return err
	}
	term.EnterAlternateScreen(out)
	defer func() {
		term.ResetModes(out)
		term.ExitAlternateScreen(out)
	}()
	defer func() {
		closeEnv := protocol.Envelope{Type: protocol.TypeTerminalClose, SessionID: sessionID, StreamID: streamID}
		_ = writeEnvelope(conn, closeEnv)
	}()

	errc := make(chan error, 2)
	go func() {
		for {
			env, err := readEnvelope(conn)
			if err != nil {
				errc <- err
				return
			}
			switch env.Type {
			case protocol.TypeTerminalOutput:
				var output protocol.TerminalOutput
				_ = env.DecodePayload(&output)
				if output.Encoding == "base64" {
					data, err := base64.StdEncoding.DecodeString(output.Data)
					if err == nil {
						_, _ = out.Write(data)
					}
					continue
				}
				fmt.Fprint(out, output.Data)
			case protocol.TypeError:
				var payload protocol.ErrorPayload
				_ = env.DecodePayload(&payload)
				errc <- errors.New(payload.Message)
				return
			}
		}
	}()
	if file, ok := in.(*os.File); ok {
		if restore, err := term.MakeRaw(file); err == nil {
			defer restore()
		}
		go watchResize(ctx, file, conn, sessionID, streamID, errc)
	}
	go func() {
		buffer := make([]byte, 1024)
		for {
			n, err := in.Read(buffer)
			if n > 0 {
				if containsDetachKey(buffer[:n]) {
					errc <- nil
					return
				}
				env, buildErr := protocol.NewEnvelope(protocol.TypeControlInput, protocol.TerminalInput{Data: string(buffer[:n])})
				if buildErr != nil {
					errc <- buildErr
					return
				}
				env.SessionID = sessionID
				env.StreamID = streamID
				if writeErr := writeEnvelope(conn, env); writeErr != nil {
					errc <- writeErr
					return
				}
			}
			if err != nil {
				errc <- err
				return
			}
		}
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errc:
		return err
	}
}

func newStreamID(sessionID string) string {
	return fmt.Sprintf("%d-%d|%s|", time.Now().UnixNano(), rand.Int63(), sessionID)
}

func terminalSize(in io.Reader) (int, int) {
	file, ok := in.(*os.File)
	if !ok {
		return 120, 36
	}
	cols, rows, err := term.Size(file)
	if err != nil {
		return 120, 36
	}
	return cols, rows
}

func watchResize(ctx context.Context, file *os.File, conn *ws.Conn, sessionID string, streamID string, errc chan<- error) {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGWINCH)
	defer signal.Stop(signals)
	for {
		select {
		case <-ctx.Done():
			return
		case <-signals:
			cols, rows, err := term.Size(file)
			if err != nil {
				continue
			}
			env, err := protocol.NewEnvelope(protocol.TypeTerminalResize, protocol.TerminalSize{Cols: cols, Rows: rows})
			if err != nil {
				errc <- err
				return
			}
			env.SessionID = sessionID
			env.StreamID = streamID
			if err := writeEnvelope(conn, env); err != nil {
				errc <- err
				return
			}
		}
	}
}

func containsDetachKey(data []byte) bool {
	for _, b := range data {
		if b == 0x1d {
			return true
		}
	}
	return false
}

func (c Client) doJSON(ctx context.Context, method, path string, body any, target any) error {
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

func controlURL(hubURL, token string) (string, error) {
	value := strings.TrimRight(hubURL, "/")
	if strings.HasPrefix(value, "http://") {
		value = "ws://" + strings.TrimPrefix(value, "http://")
	}
	if strings.HasPrefix(value, "https://") {
		value = "wss://" + strings.TrimPrefix(value, "https://")
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return "", err
	}
	if parsed.Scheme != "ws" && parsed.Scheme != "wss" {
		return "", fmt.Errorf("hub URL must use http(s) or ws(s)")
	}
	parsed.Path = "/ws/control"
	if token != "" {
		q := parsed.Query()
		q.Set("token", token)
		parsed.RawQuery = q.Encode()
	}
	return parsed.String(), nil
}

func readEnvelope(conn *ws.Conn) (protocol.Envelope, error) {
	text, err := conn.ReadText()
	if err != nil {
		return protocol.Envelope{}, err
	}
	var env protocol.Envelope
	if err := json.Unmarshal([]byte(text), &env); err != nil {
		return protocol.Envelope{}, err
	}
	return env, env.Validate()
}

func writeEnvelope(conn *ws.Conn, env protocol.Envelope) error {
	raw, err := json.Marshal(env)
	if err != nil {
		return err
	}
	return conn.WriteText(string(raw))
}

func DefaultIO() (io.Reader, io.Writer) {
	return os.Stdin, os.Stdout
}
