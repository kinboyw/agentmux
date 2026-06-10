package terminalview

import (
	"strings"
	"testing"
	"time"
)

func TestViewAppliesClearScreen(t *testing.T) {
	view := New(20, 4)
	view.Write([]byte("old line\nsecond\n\x1b[2J\x1b[Hnew"))
	got := stripANSI(view.Render())
	if strings.Contains(got, "old line") || strings.Contains(got, "second") {
		t.Fatalf("clear screen was not applied:\n%q", got)
	}
	if !strings.Contains(got, "new") {
		t.Fatalf("new screen content missing:\n%q", got)
	}
}

func TestViewAppliesCarriageReturnOverwrite(t *testing.T) {
	view := New(20, 3)
	view.Write([]byte("progress 10%\rprogress 90%"))
	got := stripANSI(view.Render())
	if strings.Contains(got, "10%") {
		t.Fatalf("carriage return overwrite was not applied:\n%q", got)
	}
	if !strings.Contains(got, "progress 90%") {
		t.Fatalf("overwritten content missing:\n%q", got)
	}
}

func TestViewPreservesSGR(t *testing.T) {
	view := New(20, 3)
	view.Write([]byte("\x1b[31mred\x1b[0m"))
	got := view.Render()
	if !strings.Contains(got, "\x1b[31m") || !strings.Contains(got, "red") {
		t.Fatalf("expected rendered color and text, got %q", got)
	}
}

func TestViewWriteDoesNotBlockOnTerminalReports(t *testing.T) {
	view := New(20, 3)
	done := make(chan struct{})
	go func() {
		defer close(done)
		view.Write([]byte("\x1b[c\x1b[6n\x1b[5n"))
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("terminal report sequences blocked view write")
	}
}

func TestMouseInputRequiresRemoteMouseMode(t *testing.T) {
	view := New(20, 3)
	if got := view.MouseInput(MouseEvent{X: 1, Y: 1, Button: MouseLeft}); got != "" {
		t.Fatalf("mouse input should be empty without remote mouse mode, got %q", got)
	}
	view.Write([]byte("\x1b[?1000h\x1b[?1006h"))
	got := view.MouseInput(MouseEvent{X: 1, Y: 1, Button: MouseLeft})
	if got != "\x1b[<0;2;2M" {
		t.Fatalf("unexpected SGR mouse input: %q", got)
	}
	view.Write([]byte("\x1b[?1000l"))
	if got := view.MouseInput(MouseEvent{X: 1, Y: 1, Button: MouseLeft}); got != "" {
		t.Fatalf("mouse input should stop after remote mouse mode reset, got %q", got)
	}
}

func TestMouseInputHonorsMotionMode(t *testing.T) {
	view := New(20, 3)
	view.Write([]byte("\x1b[?1000h\x1b[?1006h"))
	if got := view.MouseInput(MouseEvent{X: 1, Y: 1, Button: MouseLeft, Motion: true}); got != "" {
		t.Fatalf("normal mouse mode should not forward motion, got %q", got)
	}
	view.Write([]byte("\x1b[?1002h"))
	got := view.MouseInput(MouseEvent{X: 1, Y: 1, Button: MouseLeft, Motion: true})
	if got == "" {
		t.Fatalf("button motion mode should forward drag motion")
	}
}

func stripANSI(value string) string {
	var out strings.Builder
	for i := 0; i < len(value); i++ {
		if value[i] == 0x1b {
			i = skipANSI(value, i)
			continue
		}
		out.WriteByte(value[i])
	}
	return out.String()
}

func skipANSI(value string, start int) int {
	if start+1 >= len(value) {
		return start
	}
	if value[start+1] != '[' {
		return start + 1
	}
	i := start + 1
	for i+1 < len(value) {
		i++
		if value[i] >= 0x40 && value[i] <= 0x7e {
			return i
		}
	}
	return i
}
