package protocol

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	ProtocolVersion = "1"

	TypeWorkerHello        = "worker.hello"
	TypeWorkerHeartbeat    = "worker.heartbeat"
	TypeSessionSnapshot    = "session.snapshot"
	TypeSessionSync        = "session.sync"
	TypeSessionPreview     = "session.preview"
	TypeSessionTargets     = "session.targets"
	TypeSessionCreate      = "session.create"
	TypeSessionCreated     = "session.created"
	TypeSessionKill        = "session.kill"
	TypeTerminalOpen       = "terminal.open"
	TypeTerminalClose      = "terminal.close"
	TypeTerminalInput      = "terminal.input"
	TypeTerminalOutput     = "terminal.output"
	TypeTerminalResize     = "terminal.resize"
	TypeControlOpen        = "control.open"
	TypeControlInput       = "control.input"
	TypeWorkerUpdateApply  = "worker.update.apply"
	TypeWorkerUpdateResult = "worker.update.result"
	TypeError              = "error"
)

var DefaultWorkerCapabilities = []string{
	"session.snapshot",
	"session.create",
	"session.kill",
	"session.preview.active_pane",
	"session.targets",
	"terminal.open",
	"terminal.resize",
	"terminal.target_attach",
	"worker.software_inventory",
	"worker.update.apply",
}

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
	Backend string `json:"backend,omitempty"`
	WorkerSoftware
}

type WorkerSoftware struct {
	Version         string   `json:"version,omitempty"`
	Commit          string   `json:"commit,omitempty"`
	BuildTime       string   `json:"build_time,omitempty"`
	GoVersion       string   `json:"go_version,omitempty"`
	OS              string   `json:"os,omitempty"`
	Arch            string   `json:"arch,omitempty"`
	ProtocolVersion string   `json:"protocol_version,omitempty"`
	Capabilities    []string `json:"capabilities,omitempty"`
	InstallKind     string   `json:"install_kind,omitempty"`
	ServiceBackend  string   `json:"service_backend,omitempty"`
	UpdateChannel   string   `json:"update_channel,omitempty"`
	UpdatePolicy    string   `json:"update_policy,omitempty"`
}

type Session struct {
	Name    string `json:"name"`
	CWD     string `json:"cwd"`
	Command string `json:"command"`
	Status  string `json:"status"`
	Backend string `json:"backend,omitempty"`
}

type SessionView struct {
	ID       string `json:"id"`
	TenantID string `json:"tenant_id,omitempty"`
	WorkerID string `json:"worker_id"`
	Name     string `json:"name"`
	CWD      string `json:"cwd"`
	Command  string `json:"command"`
	Status   string `json:"status"`
	Backend  string `json:"backend,omitempty"`
}

type SessionSnapshot struct {
	Sessions []Session `json:"sessions"`
}

type SessionPreviewRequest struct {
	Lines int    `json:"lines"`
	Scope string `json:"scope,omitempty"`
}

type SessionPreview struct {
	Data  string `json:"data"`
	Scope string `json:"scope,omitempty"`
}

type SessionTargetsRequest struct{}

type SessionTargets struct {
	Targets []TerminalTarget `json:"targets"`
}

type TerminalTarget struct {
	SessionName  string `json:"session_name,omitempty"`
	WindowID     string `json:"window_id,omitempty"`
	WindowIndex  int    `json:"window_index,omitempty"`
	WindowName   string `json:"window_name,omitempty"`
	WindowActive bool   `json:"window_active,omitempty"`
	PaneID       string `json:"pane_id,omitempty"`
	PaneIndex    int    `json:"pane_index,omitempty"`
	PaneActive   bool   `json:"pane_active,omitempty"`
	CWD          string `json:"cwd,omitempty"`
	Command      string `json:"command,omitempty"`
	Left         int    `json:"left,omitempty"`
	Top          int    `json:"top,omitempty"`
	Width        int    `json:"width,omitempty"`
	Height       int    `json:"height,omitempty"`
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

type TerminalOpen struct {
	Cols   int             `json:"cols"`
	Rows   int             `json:"rows"`
	Target *TerminalTarget `json:"target,omitempty"`
}

func NewTerminalOpen(size TerminalSize, target *TerminalTarget) TerminalOpen {
	return TerminalOpen{Cols: size.Cols, Rows: size.Rows, Target: target}
}

func (o TerminalOpen) Size() TerminalSize {
	return TerminalSize{Cols: o.Cols, Rows: o.Rows}
}

type TerminalOutput struct {
	Data     string `json:"data"`
	Encoding string `json:"encoding,omitempty"`
}

type WorkerView struct {
	ID           string         `json:"id"`
	TenantID     string         `json:"tenant_id,omitempty"`
	Name         string         `json:"name"`
	Addr         string         `json:"addr"`
	Backend      string         `json:"backend,omitempty"`
	Software     WorkerSoftware `json:"software,omitempty"`
	LastSeen     time.Time      `json:"last_seen"`
	Status       string         `json:"status,omitempty"`
	Online       bool           `json:"online"`
	Enabled      bool           `json:"enabled"`
	TraceEnabled bool           `json:"trace_enabled"`
	DebugEnabled bool           `json:"debug_enabled"`
}

type WorkerUpdateApply struct {
	JobID                  string `json:"job_id"`
	Repo                   string `json:"repo,omitempty"`
	Version                string `json:"version"`
	Role                   string `json:"role,omitempty"`
	Restart                bool   `json:"restart"`
	AllowDisruptiveRestart bool   `json:"allow_disruptive_restart,omitempty"`
}

type WorkerUpdateResult struct {
	JobID   string `json:"job_id"`
	Status  string `json:"status"`
	Version string `json:"version,omitempty"`
	Message string `json:"message,omitempty"`
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
