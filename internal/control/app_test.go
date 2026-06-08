package control

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	"private/agentmux/internal/protocol"
)

func TestDedupeSessionsSortsByID(t *testing.T) {
	sessions := dedupeSessions([]protocol.SessionView{
		{ID: "worker/z", Status: "old"},
		{ID: "worker/a", Status: "active"},
		{ID: "worker/z", Status: "active"},
	})
	if len(sessions) != 2 {
		t.Fatalf("expected 2 sessions, got %d: %+v", len(sessions), sessions)
	}
	if sessions[0].ID != "worker/a" || sessions[1].ID != "worker/z" {
		t.Fatalf("sessions not sorted: %+v", sessions)
	}
	if sessions[1].Status != "active" {
		t.Fatalf("expected later duplicate to win: %+v", sessions[1])
	}
}

func TestDedupeWorkersKeepsNewestLastSeen(t *testing.T) {
	oldTime := time.Now().UTC().Add(-time.Hour)
	newTime := time.Now().UTC()
	workers := dedupeWorkers([]protocol.WorkerView{
		{ID: "local", Name: "old", LastSeen: oldTime},
		{ID: "local", Name: "new", LastSeen: newTime},
	})
	if len(workers) != 1 {
		t.Fatalf("expected 1 worker, got %d", len(workers))
	}
	if workers[0].Name != "new" {
		t.Fatalf("expected newest worker: %+v", workers[0])
	}
}

func TestReadAppKeyEscDoesNotQuit(t *testing.T) {
	key, err := readAppKey(strings.NewReader("\x1b"))
	if err != nil {
		t.Fatal(err)
	}
	if key != "unknown" {
		t.Fatalf("expected bare esc to be ignored, got %q", key)
	}
}

func TestReadAppKeyArrows(t *testing.T) {
	key, err := readAppKey(strings.NewReader("\x1b[A"))
	if err != nil {
		t.Fatal(err)
	}
	if key != "up" {
		t.Fatalf("expected up, got %q", key)
	}
	key, err = readAppKey(strings.NewReader("\x1b[B"))
	if err != nil {
		t.Fatal(err)
	}
	if key != "down" {
		t.Fatalf("expected down, got %q", key)
	}
	key, err = readAppKey(strings.NewReader("\x1bOB"))
	if err != nil {
		t.Fatal(err)
	}
	if key != "down" {
		t.Fatalf("expected application cursor down, got %q", key)
	}
}

func TestAppKeyReaderHandlesSplitArrowSequence(t *testing.T) {
	reader := &chunkReader{chunks: [][]byte{
		[]byte("\x1b"),
		[]byte("["),
		[]byte("B"),
	}}
	var keys appKeyReader
	key, err := keys.Read(reader)
	if err != nil {
		t.Fatal(err)
	}
	if key != "down" {
		t.Fatalf("expected split down arrow, got %q", key)
	}
}

func TestVisibleLenASCIIStyledIcons(t *testing.T) {
	line := styleAccent("> ") + "local/demo " + styleOK("* active")
	if got, want := visibleLen(line), len("> local/demo * active"); got != want {
		t.Fatalf("unexpected visible len: got %d want %d", got, want)
	}
}

func TestAppKeyReaderResetClearsPendingBytes(t *testing.T) {
	var keys appKeyReader
	key, err := keys.Read(strings.NewReader("\x1b["))
	if err != nil {
		t.Fatal(err)
	}
	if key != "unknown" {
		t.Fatalf("expected unknown, got %q", key)
	}
	keys.Reset()
	key, err = keys.Read(strings.NewReader("\x1b[B"))
	if err != nil {
		t.Fatal(err)
	}
	if key != "down" {
		t.Fatalf("expected down after reset, got %q", key)
	}
}

func TestSplitPreviewLinesUsesTail(t *testing.T) {
	lines := splitPreviewLines("one\ntwo\nthree\n", 2)
	if strings.Join(lines, ",") != "two,three" {
		t.Fatalf("unexpected preview lines: %+v", lines)
	}
}

func TestStripControlRemovesEscapeSequences(t *testing.T) {
	got := stripControl("\x1b[31mred\x1b[0m\x03")
	if got != "red" {
		t.Fatalf("unexpected stripped line: %q", got)
	}
}

func TestSanitizePreviewANSIKeepsSGR(t *testing.T) {
	got := sanitizePreviewANSI("\x1b[31mred\x1b[0m\x1b[2J")
	if !strings.Contains(got, "\x1b[31mred\x1b[0m") {
		t.Fatalf("expected SGR color to remain: %q", got)
	}
	if strings.Contains(got, "\x1b[2J") {
		t.Fatalf("non-SGR escape should be removed: %q", got)
	}
}

func TestHighlightPreviewLinePreservesExistingSGR(t *testing.T) {
	input := "\x1b[31mred\x1b[0m"
	if got := highlightPreviewLine(input); got != input {
		t.Fatalf("existing SGR should be preserved: %q", got)
	}
}

func TestHighlightPreviewLineAddsFallbackColor(t *testing.T) {
	if got := highlightPreviewLine("error: failed"); !strings.Contains(got, "\x1b[31m") {
		t.Fatalf("expected error highlight: %q", got)
	}
	if got := highlightPreviewLine("ready"); !strings.Contains(got, "\x1b[32m") {
		t.Fatalf("expected ok highlight: %q", got)
	}
	if got := highlightPreviewLine("$ pwd"); !strings.Contains(got, "\x1b[36m") {
		t.Fatalf("expected prompt highlight: %q", got)
	}
}

func TestCompactSessionLineFitsNarrowList(t *testing.T) {
	line := compactSessionLine("> ", protocol.SessionView{
		ID:      "worker/very-long-session-name",
		Status:  "active",
		Command: "very-long-command",
	})
	if visibleLen(line) > 42 {
		t.Fatalf("compact line too wide: %d %q", visibleLen(line), line)
	}
	if !strings.Contains(stripControl(line), "worker/") {
		t.Fatalf("session id disappeared: %q", line)
	}
	if strings.Contains(stripControl(line), "\x1b") {
		t.Fatalf("compact line should not embed raw escapes in visible text: %q", line)
	}
	if !strings.Contains(line, "~") {
		t.Fatalf("expected ellipsis marker: %q", line)
	}
}

func TestPreviewLineCanBeTruncatedToWidth(t *testing.T) {
	line := previewLine([]string{"\x1b[31m" + strings.Repeat("x", 80) + "\x1b[0m"}, 0)
	got := truncateVisible(line, 20)
	if visibleLen(got) > 20 {
		t.Fatalf("preview line too wide: %d", visibleLen(got))
	}
}

func TestRenderIncludesSessionListWhenPreviewExists(t *testing.T) {
	app := &App{
		Out: &bytes.Buffer{},
		sessions: []protocol.SessionView{{
			ID: "local/demo", WorkerID: "local", Name: "demo", Status: "active", Command: "bash",
		}},
		preview: "colored \x1b[31merror\x1b[0m",
		status:  "ready",
	}
	app.renderWithSize(120, 24)
	output := stripControl(app.Out.(*bytes.Buffer).String())
	if !strings.Contains(output, "local/demo") {
		t.Fatalf("session list missing from render:\n%s", output)
	}
	if !strings.Contains(output, "colored error") {
		t.Fatalf("preview missing from render:\n%s", output)
	}
}

type chunkReader struct {
	chunks [][]byte
	index  int
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if r.index >= len(r.chunks) {
		return 0, io.EOF
	}
	chunk := r.chunks[r.index]
	r.index++
	copy(p, chunk)
	return min(len(p), len(chunk)), nil
}
