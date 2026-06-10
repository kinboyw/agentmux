package control

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"private/agentmux/internal/protocol"
)

const defaultDebugLogName = "agentmux-tui-debug.log"

type appDebugState struct {
	enabled            bool
	logPath            string
	logFile            *os.File
	startedAt          time.Time
	lastKey            string
	lastRenderAt       time.Time
	lastRenderCols     int
	lastRenderRows     int
	renderCount        int64
	streamEvents       int64
	streamBytes        int64
	streamErrors       int64
	pendingProcessed   int64
	pendingDropped     int64
	lastPendingSession string
	lastPendingBytes   int
	lastResizeSession  string
	lastResize         protocol.TerminalSize
}

type AppDebugOptions struct {
	Enabled bool
	LogPath string
}

func (a *App) EnableDebug(options AppDebugOptions) error {
	if !options.Enabled && strings.TrimSpace(options.LogPath) == "" {
		return nil
	}
	if a.debug.logFile != nil {
		_ = a.debug.logFile.Close()
		a.debug.logFile = nil
	}
	path := strings.TrimSpace(options.LogPath)
	if path == "" {
		path = filepath.Join(os.TempDir(), defaultDebugLogName)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	a.debug.enabled = true
	a.debug.logPath = path
	a.debug.logFile = file
	a.debug.startedAt = time.Now()
	a.debugf("debug enabled log=%q", path)
	return nil
}

func (a *App) debugEnabled() bool {
	return a != nil && a.debug.enabled
}

func (a *App) closeDebug() {
	if a == nil || a.debug.logFile == nil {
		return
	}
	_ = a.debug.logFile.Close()
	a.debug.logFile = nil
}

func (a *App) debugf(format string, args ...any) {
	if !a.debugEnabled() || a.debug.logFile == nil {
		return
	}
	line := fmt.Sprintf(format, args...)
	_, _ = fmt.Fprintf(a.debug.logFile, "%s %s\n", time.Now().Format(time.RFC3339Nano), line)
}

func (a *App) recordDebugKey(key string, activeInput bool) {
	if !a.debugEnabled() {
		return
	}
	label := debugKeyLabel(key)
	if activeInput && isDebugPrintableInputKey(key) {
		label = "input"
	}
	a.debug.lastKey = label
	a.debugf("key %q", label)
}

func (a *App) recordDebugRender(cols, rows int) {
	if !a.debugEnabled() {
		return
	}
	a.debug.renderCount++
	a.debug.lastRenderAt = time.Now()
	a.debug.lastRenderCols = cols
	a.debug.lastRenderRows = rows
}

func (a *App) recordDebugStreamEvent(event appStreamEvent) {
	if !a.debugEnabled() {
		return
	}
	streamID := ""
	if event.stream != nil {
		streamID = event.stream.StreamID
	}
	a.debug.streamEvents++
	a.debug.streamBytes += int64(len(event.data))
	if event.err != nil {
		a.debug.streamErrors++
		a.debugf("stream event session=%q stream_id=%q bytes=%d connected=%t closed=%t error=%q", event.sessionID, streamID, len(event.data), event.connected, event.closed, event.err.Error())
		return
	}
	if event.connected || event.closed || len(event.data) > 0 {
		a.debugf("stream event session=%q stream_id=%q bytes=%d connected=%t closed=%t", event.sessionID, streamID, len(event.data), event.connected, event.closed)
	}
}

func (a *App) recordDebugPendingProcessed(sessionID string, processed int, remaining int) {
	if !a.debugEnabled() {
		return
	}
	a.debug.pendingProcessed += int64(processed)
	a.debug.lastPendingSession = sessionID
	a.debug.lastPendingBytes = remaining
}

func (a *App) recordDebugPendingDropped(sessionID string, dropped int) {
	if !a.debugEnabled() {
		return
	}
	a.debug.pendingDropped += int64(dropped)
	a.debugf("pending dropped session=%q bytes=%d", sessionID, dropped)
}

func (a *App) recordDebugResize(sessionID string, size protocol.TerminalSize) {
	if !a.debugEnabled() {
		return
	}
	a.debug.lastResizeSession = sessionID
	a.debug.lastResize = size
	a.debugf("resize session=%q cols=%d rows=%d", sessionID, size.Cols, size.Rows)
}

func (a *App) debugHUD() string {
	parts := []string{
		styleWarn("debug"),
		fmt.Sprintf("renders=%d", a.debug.renderCount),
		fmt.Sprintf("events=%d", a.debug.streamEvents),
		fmt.Sprintf("bytes=%d", a.debug.streamBytes),
		fmt.Sprintf("pending=%d", a.totalPendingBytes()),
	}
	if a.debug.lastKey != "" {
		parts = append(parts, "key="+debugKeyLabel(a.debug.lastKey))
	}
	return strings.Join(parts, " ")
}

func (a *App) writeDebugSnapshot(reason string) (string, error) {
	if !a.debugEnabled() {
		return "", fmt.Errorf("debug mode is not enabled")
	}
	path := a.debug.logPath
	if path == "" {
		path = filepath.Join(os.TempDir(), defaultDebugLogName)
	}
	snapshot := a.debugSnapshot(reason)
	file := a.debug.logFile
	if file == nil {
		var err error
		file, err = os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return "", err
		}
		defer file.Close()
	}
	if _, err := fmt.Fprintln(file, "SNAPSHOT "+string(snapshot)); err != nil {
		return "", err
	}
	return path, nil
}

func (a *App) debugSnapshot(reason string) []byte {
	type streamSnapshot struct {
		SessionID  string                `json:"session_id"`
		StreamID   string                `json:"stream_id,omitempty"`
		Connected  bool                  `json:"connected"`
		Connecting bool                  `json:"connecting"`
		Warming    bool                  `json:"warming"`
		Closing    bool                  `json:"closing"`
		SeenOutput bool                  `json:"seen_output"`
		Visible    bool                  `json:"visible_output"`
		Prefilled  bool                  `json:"prefilled"`
		Pending    int                   `json:"pending_bytes"`
		Size       protocol.TerminalSize `json:"size"`
		KeepUntil  string                `json:"keep_until,omitempty"`
	}
	streams := make([]streamSnapshot, 0, len(a.streams))
	for _, sessionID := range sortedStreamIDs(a.streams) {
		stream := a.streams[sessionID]
		if stream == nil {
			continue
		}
		item := streamSnapshot{
			SessionID:  sessionID,
			Connected:  stream.stream != nil,
			Connecting: stream.connecting,
			Warming:    stream.warming,
			Closing:    stream.closing,
			SeenOutput: stream.seenOutput,
			Visible:    stream.visibleOutput,
			Prefilled:  stream.prefilled,
			Pending:    len(stream.pending),
			Size:       stream.size,
		}
		if stream.stream != nil {
			item.StreamID = stream.stream.StreamID
		}
		if !stream.keepUntil.IsZero() {
			item.KeepUntil = stream.keepUntil.Format(time.RFC3339Nano)
		}
		streams = append(streams, item)
	}
	selected := a.selectedSession()
	payload := map[string]any{
		"reason":             reason,
		"at":                 time.Now().Format(time.RFC3339Nano),
		"started_at":         a.debug.startedAt.Format(time.RFC3339Nano),
		"last_render_at":     a.debug.lastRenderAt.Format(time.RFC3339Nano),
		"hub":                debugSafeURL(a.Client.HubURL),
		"logged_in":          a.loggedIn,
		"auth_source":        a.Auth.Source,
		"tenant_id":          a.Auth.TenantID,
		"workers":            len(a.workers),
		"sessions":           len(a.sessions),
		"selected_index":     a.selected,
		"selected_session":   selected.ID,
		"active_session":     a.active,
		"fullscreen":         a.fullscreen,
		"status":             stripControl(a.status),
		"error":              debugError(a.err),
		"preview_bytes":      len(a.preview),
		"event_queue":        len(a.events),
		"buffered_sessions":  len(a.buffers),
		"render_count":       a.debug.renderCount,
		"last_render_cols":   a.debug.lastRenderCols,
		"last_render_rows":   a.debug.lastRenderRows,
		"last_key":           debugKeyLabel(a.debug.lastKey),
		"stream_events":      a.debug.streamEvents,
		"stream_bytes":       a.debug.streamBytes,
		"stream_errors":      a.debug.streamErrors,
		"pending_processed":  a.debug.pendingProcessed,
		"pending_dropped":    a.debug.pendingDropped,
		"total_pending":      a.totalPendingBytes(),
		"last_pending_id":    a.debug.lastPendingSession,
		"last_pending_bytes": a.debug.lastPendingBytes,
		"last_resize_id":     a.debug.lastResizeSession,
		"last_resize":        a.debug.lastResize,
		"streams":            streams,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return []byte(`{"error":"snapshot marshal failed"}`)
	}
	return raw
}

func (a *App) totalPendingBytes() int {
	if a == nil {
		return 0
	}
	total := 0
	for _, stream := range a.streams {
		if stream != nil {
			total += len(stream.pending)
		}
	}
	return total
}

func sortedStreamIDs(streams map[string]*appSessionStream) []string {
	ids := make([]string, 0, len(streams))
	for id := range streams {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func streamKeepUntil(stream *appSessionStream) string {
	if stream == nil || stream.keepUntil.IsZero() {
		return ""
	}
	return stream.keepUntil.Format(time.RFC3339Nano)
}

func debugError(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func debugKeyLabel(key string) string {
	if key == "" {
		return ""
	}
	if len(key) == 1 && key[0] < 0x20 {
		return fmt.Sprintf("0x%02x", key[0])
	}
	if len(key) == 1 && key[0] == 0x7f {
		return "0x7f"
	}
	return key
}

func isDebugPrintableInputKey(key string) bool {
	return len(key) == 1 && key[0] >= 0x20 && key[0] != 0x7f
}

func debugSafeURL(value string) string {
	parsed, err := url.Parse(value)
	if err != nil {
		if i := strings.IndexAny(value, "?#"); i >= 0 {
			return value[:i]
		}
		return value
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}
