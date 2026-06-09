package ptybackend

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestBackendCreateInputCaptureKill(t *testing.T) {
	backend := New()
	ctx := context.Background()
	if err := backend.Create(ctx, "demo", t.TempDir(), "cat"); err != nil {
		t.Fatal(err)
	}
	if err := backend.SendTerminalInput(ctx, "demo", "hello\n"); err != nil {
		t.Fatal(err)
	}
	var preview string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var err error
		preview, err = backend.Capture(ctx, "demo", 20)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(preview, "hello") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(preview, "hello") {
		t.Fatalf("preview did not include input: %q", preview)
	}
	sessions, err := backend.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].Name != "demo" || sessions[0].Status != "idle" {
		t.Fatalf("unexpected sessions: %+v", sessions)
	}
	if err := backend.Kill(ctx, "demo"); err != nil {
		t.Fatal(err)
	}
}
