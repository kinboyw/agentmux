package control

import (
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
