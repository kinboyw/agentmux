package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"private/agentmux/internal/protocol"
	"private/agentmux/internal/ws"
)

type Stream struct {
	conn      *ws.Conn
	SessionID string
	StreamID  string
}

type StreamEvent struct {
	Type string
	Data []byte
	Err  error
}

func (c *Client) OpenStream(ctx context.Context, sessionID string, size protocol.TerminalSize) (*Stream, error) {
	return c.OpenTargetStream(ctx, sessionID, size, nil)
}

func (c *Client) OpenTargetStream(ctx context.Context, sessionID string, size protocol.TerminalSize, terminalTarget *protocol.TerminalTarget) (*Stream, error) {
	if c == nil {
		return nil, fmt.Errorf("control client is nil")
	}
	if err := c.EnsureFresh(ctx); err != nil {
		return nil, err
	}
	target, err := controlURL(c.HubURL, c.Token)
	if err != nil {
		return nil, err
	}
	conn, err := ws.Dial(ctx, target, c.Token)
	if err != nil {
		return nil, err
	}
	stream := &Stream{conn: conn, SessionID: sessionID, StreamID: newStreamID(sessionID)}
	open, err := protocol.NewEnvelope(protocol.TypeControlOpen, protocol.NewTerminalOpen(size, terminalTarget))
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	open.SessionID = sessionID
	open.StreamID = stream.StreamID
	if err := writeEnvelope(conn, open); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return stream, nil
}

func (s *Stream) ReadEvent() (StreamEvent, error) {
	env, err := readEnvelope(s.conn)
	if err != nil {
		return StreamEvent{}, err
	}
	switch env.Type {
	case protocol.TypeTerminalOutput:
		var output protocol.TerminalOutput
		_ = env.DecodePayload(&output)
		if output.Encoding == "base64" {
			data, err := base64.StdEncoding.DecodeString(output.Data)
			if err != nil {
				return StreamEvent{}, err
			}
			return StreamEvent{Type: env.Type, Data: data}, nil
		}
		return StreamEvent{Type: env.Type, Data: []byte(output.Data)}, nil
	case protocol.TypeError:
		var payload protocol.ErrorPayload
		_ = env.DecodePayload(&payload)
		return StreamEvent{Type: env.Type, Err: errors.New(payload.Message)}, nil
	default:
		return StreamEvent{Type: env.Type}, nil
	}
}

func (s *Stream) Input(data string) error {
	env, err := protocol.NewEnvelope(protocol.TypeControlInput, protocol.TerminalInput{Data: data})
	if err != nil {
		return err
	}
	env.SessionID = s.SessionID
	env.StreamID = s.StreamID
	return writeEnvelope(s.conn, env)
}

func (s *Stream) Resize(size protocol.TerminalSize) error {
	env, err := protocol.NewEnvelope(protocol.TypeTerminalResize, size)
	if err != nil {
		return err
	}
	env.SessionID = s.SessionID
	env.StreamID = s.StreamID
	return writeEnvelope(s.conn, env)
}

func (s *Stream) Close() error {
	closeEnv := protocol.Envelope{Type: protocol.TypeTerminalClose, SessionID: s.SessionID, StreamID: s.StreamID}
	_ = writeEnvelope(s.conn, closeEnv)
	return s.conn.Close()
}

func newStreamID(sessionID string) string {
	return fmt.Sprintf("%d-%d|%s|", time.Now().UnixNano(), randomInt63(), sessionID)
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
