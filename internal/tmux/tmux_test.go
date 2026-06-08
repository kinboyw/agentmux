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
	if sessions[1].Status != "attached" {
		t.Fatalf("unexpected status: %+v", sessions[1])
	}
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
	if err := adapter.SendTerminalInput(context.Background(), "demo", "ls\r\x7f\x1b[A"); err != nil {
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
