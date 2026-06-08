package protocol

import "testing"

func TestSplitSessionID(t *testing.T) {
	worker, name, ok := SplitSessionID("local/demo")
	if !ok || worker != "local" || name != "demo" {
		t.Fatalf("unexpected split: %q %q %v", worker, name, ok)
	}

	_, _, ok = SplitSessionID("broken")
	if ok {
		t.Fatal("expected invalid id")
	}
}

func TestEnvelopePayloadRoundTrip(t *testing.T) {
	env, err := NewEnvelope(TypeTerminalInput, TerminalInput{Data: "pwd\n"})
	if err != nil {
		t.Fatal(err)
	}
	var input TerminalInput
	if err := env.DecodePayload(&input); err != nil {
		t.Fatal(err)
	}
	if input.Data != "pwd\n" {
		t.Fatalf("unexpected payload: %q", input.Data)
	}
}
