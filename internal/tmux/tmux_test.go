package tmux

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"private/agentmux/internal/sessionbackend"
)

type call struct {
	name string
	args []string
}

type fakeRunner struct {
	output string
	calls  []call
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	f.calls = append(f.calls, call{name: name, args: append([]string(nil), args...)})
	return f.output, nil
}

type fakeRunnerFunc func(context.Context, string, ...string) (string, error)

func (f fakeRunnerFunc) Run(ctx context.Context, name string, args ...string) (string, error) {
	return f(ctx, name, args...)
}

func TestListParsesTmuxPanes(t *testing.T) {
	runner := &fakeRunner{output: "demo\t/tmp\tbash\t0\nattached\t/repo\tcodex\t1\n"}
	adapter := New(runner)
	sessions, err := adapter.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(sessions))
	}
	if sessionByName(sessions, "attached").Status != "attached" {
		t.Fatalf("unexpected status: %+v", sessions)
	}
}

func sessionByName(sessions []sessionbackend.Session, name string) sessionbackend.Session {
	for _, session := range sessions {
		if session.Name == name {
			return session
		}
	}
	return sessionbackend.Session{}
}

func TestSendInputSplitsNewline(t *testing.T) {
	runner := &fakeRunner{}
	adapter := New(runner)
	if err := adapter.SendInput(context.Background(), "demo", "pwd\n"); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("expected literal and enter calls, got %d", len(runner.calls))
	}
	if strings.Join(runner.calls[0].args, " ") != "send-keys -t demo -l pwd" {
		t.Fatalf("unexpected literal call: %#v", runner.calls[0].args)
	}
	if strings.Join(runner.calls[1].args, " ") != "send-keys -t demo C-m" {
		t.Fatalf("unexpected enter call: %#v", runner.calls[1].args)
	}
}

func TestSendTerminalInputTranslatesControlKeys(t *testing.T) {
	runner := &fakeRunner{}
	adapter := New(runner)
	if err := adapter.SendTerminalInput(context.Background(), "demo", "ls\r\x7f\x1b[A\x14\x1b"); err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(runner.calls))
	for _, call := range runner.calls {
		got = append(got, strings.Join(call.args, " "))
	}
	want := []string{
		"send-keys -t demo -l ls",
		"send-keys -t demo C-m",
		"send-keys -t demo BSpace",
		"send-keys -t demo Up",
		"send-keys -t demo C-t",
		"send-keys -t demo Escape",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("unexpected calls:\n%s", strings.Join(got, "\n"))
	}
}

func TestSendTerminalInputPreservesUTF8Literal(t *testing.T) {
	runner := &fakeRunner{}
	adapter := New(runner)
	if err := adapter.SendTerminalInput(context.Background(), "demo", "echo 中文\r"); err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(runner.calls))
	for _, call := range runner.calls {
		got = append(got, strings.Join(call.args, " "))
	}
	want := []string{
		"send-keys -t demo -l echo 中文",
		"send-keys -t demo C-m",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("unexpected calls:\n%s", strings.Join(got, "\n"))
	}
}

func TestCreateRejectsBadName(t *testing.T) {
	adapter := New(&fakeRunner{})
	if err := adapter.Create(context.Background(), "bad name", ".", "bash"); err == nil {
		t.Fatal("expected invalid name error")
	}
}

func TestCapturePreservesEscapeSequences(t *testing.T) {
	runner := fakeRunnerFunc(func(_ context.Context, name string, args ...string) (string, error) {
		got := strings.Join(args, " ")
		switch got {
		case "list-panes -t demo -F " + tmuxPaneGeometryFormat:
			return "%1\t0\t0\t80\t24\t80\t24\n", nil
		case "capture-pane -t demo -p -e -S -12":
			return "\x1b[31mred\x1b[0m", nil
		default:
			return "", fmt.Errorf("unexpected call: %s %s", name, got)
		}
	})
	adapter := New(runner)
	output, err := adapter.Capture(context.Background(), "demo", 12)
	if err != nil {
		t.Fatal(err)
	}
	if output != "\x1b[31mred\x1b[0m" {
		t.Fatalf("unexpected output: %q", output)
	}
}

func TestCaptureComposesSplitWindowPanes(t *testing.T) {
	calls := []string(nil)
	runner := fakeRunnerFunc(func(_ context.Context, name string, args ...string) (string, error) {
		got := strings.Join(args, " ")
		calls = append(calls, got)
		switch got {
		case "list-panes -t demo -F " + tmuxPaneGeometryFormat:
			return "%1\t0\t0\t14\t4\t28\t4\n%2\t14\t0\t14\t4\t28\t4\n", nil
		case "capture-pane -t %1 -p -e":
			return "left one\nleft two\n", nil
		case "capture-pane -t %2 -p -e":
			return "\x1b[31mright one\x1b[0m\nright two\n", nil
		default:
			return "", fmt.Errorf("unexpected call: %s %s", name, got)
		}
	})
	adapter := New(runner)
	output, err := adapter.Capture(context.Background(), "demo", 12)
	if err != nil {
		t.Fatal(err)
	}
	plain := stripEscapeSequences(output)
	if !strings.Contains(plain, "left one") || !strings.Contains(plain, "right one") {
		t.Fatalf("expected both pane outputs in composed preview:\n%s", plain)
	}
	if !strings.Contains(plain, "+------------++------------+") {
		t.Fatalf("expected pane borders in composed preview:\n%s", plain)
	}
	if strings.Contains(plain, "\x1b") {
		t.Fatalf("expected composed preview to strip escape sequences:\n%q", plain)
	}
	wantCalls := []string{
		"list-panes -t demo -F " + tmuxPaneGeometryFormat,
		"capture-pane -t %1 -p -e",
		"capture-pane -t %2 -p -e",
	}
	if strings.Join(calls, "\n") != strings.Join(wantCalls, "\n") {
		t.Fatalf("unexpected calls:\n%s", strings.Join(calls, "\n"))
	}
}

func TestCaptureFallsBackToPaneScrollbackWhenGeometryFails(t *testing.T) {
	calls := []string(nil)
	runner := fakeRunnerFunc(func(_ context.Context, _ string, args ...string) (string, error) {
		got := strings.Join(args, " ")
		calls = append(calls, got)
		switch got {
		case "list-panes -t demo -F " + tmuxPaneGeometryFormat:
			return "", fmt.Errorf("tmux missing")
		case "capture-pane -t demo -p -e -S -12":
			return "fallback", nil
		default:
			return "", fmt.Errorf("unexpected call: %s", got)
		}
	})
	adapter := New(runner)
	output, err := adapter.Capture(context.Background(), "demo", 12)
	if err != nil {
		t.Fatal(err)
	}
	if output != "fallback" {
		t.Fatalf("unexpected output: %q", output)
	}
	if len(calls) != 2 {
		t.Fatalf("expected geometry then fallback calls, got %d: %v", len(calls), calls)
	}
}
