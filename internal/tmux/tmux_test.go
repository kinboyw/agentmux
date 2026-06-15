package tmux

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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

func TestResolvePathUsesConfiguredTmuxPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tmux")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTMUX_TMUX", path)
	got, err := ResolvePath()
	if err != nil {
		t.Fatal(err)
	}
	if got != path {
		t.Fatalf("unexpected tmux path: %q", got)
	}
}

func TestResolvePathRejectsConfiguredNonExecutable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tmux")
	if err := os.WriteFile(path, []byte("not executable"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTMUX_TMUX", path)
	if _, err := ResolvePath(); err == nil || !strings.Contains(err.Error(), "not executable") {
		t.Fatalf("expected non-executable error, got %v", err)
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

func TestCreateRejectsMissingWorkingDirectory(t *testing.T) {
	called := false
	adapter := New(fakeRunnerFunc(func(context.Context, string, ...string) (string, error) {
		called = true
		return "", nil
	}))
	err := adapter.Create(context.Background(), "demo", filepath.Join(t.TempDir(), "missing"), "bash")
	if err == nil || !strings.Contains(err.Error(), "working directory") {
		t.Fatalf("expected working directory error, got %v", err)
	}
	if called {
		t.Fatal("expected missing working directory to fail before tmux is invoked")
	}
}

func TestCreateIncludesTmuxOutputOnFailure(t *testing.T) {
	adapter := New(fakeRunnerFunc(func(context.Context, string, ...string) (string, error) {
		return "duplicate session: demo\n", fmt.Errorf("exit status 1")
	}))
	err := adapter.Create(context.Background(), "demo", t.TempDir(), "bash")
	if err == nil || !strings.Contains(err.Error(), "duplicate session: demo") {
		t.Fatalf("expected tmux output in error, got %v", err)
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

func TestOpenTargetTakesOverPanePipe(t *testing.T) {
	calls := []string(nil)
	runner := fakeRunnerFunc(func(_ context.Context, _ string, args ...string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		return "", nil
	})
	adapter := New(runner)
	stream, err := adapter.OpenTarget(context.Background(), sessionbackend.TerminalTarget{SessionName: "demo", PaneID: "%1"}, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	if stream == nil {
		t.Fatal("expected pane stream")
	}
	defer stream.Close()

	if len(calls) != 1 {
		t.Fatalf("expected one pipe-pane call, got %d: %v", len(calls), calls)
	}
	got := calls[0]
	if !strings.HasPrefix(got, "pipe-pane -t %1 cat > ") {
		t.Fatalf("unexpected pipe-pane call: %s", got)
	}
	if strings.Contains(got, " -o ") || strings.HasPrefix(got, "pipe-pane -o ") {
		t.Fatalf("pane attach should take over output pipe, got: %s", got)
	}
}

func TestCaptureTargetScreenUsesVisiblePaneOnly(t *testing.T) {
	runner := fakeRunnerFunc(func(_ context.Context, _ string, args ...string) (string, error) {
		got := strings.Join(args, " ")
		if got != "capture-pane -t %1 -p -e -S 0 -E -" {
			return "", fmt.Errorf("unexpected call: %s", got)
		}
		return "visible\nprompt $", nil
	})
	adapter := New(runner)
	output, err := adapter.CaptureTargetScreen(context.Background(), sessionbackend.TerminalTarget{SessionName: "demo", PaneID: "%1"})
	if err != nil {
		t.Fatal(err)
	}
	if output != "visible\nprompt $" {
		t.Fatalf("unexpected output: %q", output)
	}
}

func TestTargetsParsesSessionWindowsAndPanes(t *testing.T) {
	runner := fakeRunnerFunc(func(_ context.Context, name string, args ...string) (string, error) {
		got := strings.Join(args, " ")
		if got != "list-panes -s -t demo -F "+tmuxTargetFormat {
			return "", fmt.Errorf("unexpected call: %s %s", name, got)
		}
		return strings.Join([]string{
			"demo\t@2\t1\tapi\t0\t%4\t0\t1\t/repo\tbash\t0\t0\t80\t24",
			"demo\t@1\t0\tmain\t1\t%2\t1\t0\t/repo\tvim\t40\t0\t40\t24",
			"demo\t@1\t0\tmain\t1\t%1\t0\t1\t/repo\tcodex\t0\t0\t40\t24",
		}, "\n"), nil
	})
	adapter := New(runner)
	targets, err := adapter.Targets(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 3 {
		t.Fatalf("expected 3 targets, got %d", len(targets))
	}
	if targets[0].PaneID != "%1" || targets[1].PaneID != "%2" || targets[2].PaneID != "%4" {
		t.Fatalf("targets not sorted by window/pane: %+v", targets)
	}
	if !targets[0].WindowActive || !targets[0].PaneActive || targets[0].Command != "codex" || targets[2].WindowName != "api" {
		t.Fatalf("target metadata not parsed: %+v", targets)
	}
}
