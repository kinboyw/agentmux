package tmux

import (
	"context"
	"strings"
	"testing"
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

func sessionByName(sessions []Session, name string) Session {
	for _, session := range sessions {
		if session.Name == name {
			return session
		}
	}
	return Session{}
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

func TestCreateRejectsBadName(t *testing.T) {
	adapter := New(&fakeRunner{})
	if err := adapter.Create(context.Background(), "bad name", ".", "bash"); err == nil {
		t.Fatal("expected invalid name error")
	}
}

func TestCapturePreservesEscapeSequences(t *testing.T) {
	runner := &fakeRunner{output: "\x1b[31mred\x1b[0m"}
	adapter := New(runner)
	output, err := adapter.Capture(context.Background(), "demo", 12)
	if err != nil {
		t.Fatal(err)
	}
	if output != runner.output {
		t.Fatalf("unexpected output: %q", output)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("expected one call, got %d", len(runner.calls))
	}
	got := strings.Join(runner.calls[0].args, " ")
	want := "capture-pane -t demo -p -e -S -12"
	if got != want {
		t.Fatalf("unexpected capture call:\n got: %s\nwant: %s", got, want)
	}
}
