package protocol

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	TypeWorkerHello     = "worker.hello"
	TypeWorkerHeartbeat = "worker.heartbeat"
	TypeSessionSnapshot = "session.snapshot"
	TypeSessionCreate   = "session.create"
	TypeSessionKill     = "session.kill"
	TypeTerminalOpen    = "terminal.open"
	TypeTerminalClose   = "terminal.close"
	TypeTerminalInput   = "terminal.input"
	TypeTerminalOutput  = "terminal.output"
	TypeTerminalResize  = "terminal.resize"
	TypeControlOpen     = "control.open"
	TypeControlInput    = "control.input"
	TypeError           = "error"
)

type Envelope struct {
	Type      string          `json:"type"`
	ID        string          `json:"id,omitempty"`
	StreamID  string          `json:"stream_id,omitempty"`
	WorkerID  string          `json:"worker_id,omitempty"`
	SessionID string          `json:"session_id,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

func NewEnvelope(messageType string, payload any) (Envelope, error) {
	raw, err := MarshalPayload(payload)
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{Type: messageType, Payload: raw}, nil
}

func MarshalPayload(value any) (json.RawMessage, error) {
	if value == nil {
		return nil, nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

func (e Envelope) DecodePayload(target any) error {
	if len(e.Payload) == 0 {
		return nil
	}
	return json.Unmarshal(e.Payload, target)
}

func (e Envelope) Validate() error {
	if strings.TrimSpace(e.Type) == "" {
		return fmt.Errorf("message type is required")
	}
	return nil
}

type WorkerHello struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type Session struct {
	Name    string `json:"name"`
	CWD     string `json:"cwd"`
	Command string `json:"command"`
	Status  string `json:"status"`
}

type SessionView struct {
	ID       string `json:"id"`
	WorkerID string `json:"worker_id"`
	Name     string `json:"name"`
	CWD      string `json:"cwd"`
	Command  string `json:"command"`
	Status   string `json:"status"`
}

type SessionSnapshot struct {
	Sessions []Session `json:"sessions"`
}

type CreateSession struct {
	WorkerID string `json:"worker_id"`
	Name     string `json:"name"`
	CWD      string `json:"cwd"`
	Command  string `json:"command"`
}

type TerminalInput struct {
	Data string `json:"data"`
}

type TerminalSize struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

type TerminalOutput struct {
	Data     string `json:"data"`
	Encoding string `json:"encoding,omitempty"`
}

type WorkerView struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Addr     string    `json:"addr"`
	LastSeen time.Time `json:"last_seen"`
}

type ErrorPayload struct {
	Message string `json:"message"`
}

func SessionID(workerID, name string) string {
	return workerID + "/" + name
}

func SplitSessionID(id string) (workerID string, name string, ok bool) {
	workerID, name, found := strings.Cut(id, "/")
	if !found || workerID == "" || name == "" {
		return "", "", false
	}
	return workerID, name, true
}
