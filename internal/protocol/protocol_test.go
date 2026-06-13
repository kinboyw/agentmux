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

func TestTerminalSizeControlPayloadRoundTrip(t *testing.T) {
	env, err := NewEnvelope(TypeTerminalSizeSync, TerminalSizeSync{Cols: 100, Rows: 32, Source: "control_viewport"})
	if err != nil {
		t.Fatal(err)
	}
	var sync TerminalSizeSync
	if err := env.DecodePayload(&sync); err != nil {
		t.Fatal(err)
	}
	if sync.Cols != 100 || sync.Rows != 32 || sync.Source != "control_viewport" {
		t.Fatalf("unexpected size sync payload: %+v", sync)
	}
}

func TestTerminalRenderModePayloadRoundTrip(t *testing.T) {
	env, err := NewEnvelope(TypeControlOpen, TerminalOpen{
		Cols:       120,
		Rows:       36,
		RenderMode: RenderModeWorkerStateXterm,
	})
	if err != nil {
		t.Fatal(err)
	}
	var open TerminalOpen
	if err := env.DecodePayload(&open); err != nil {
		t.Fatal(err)
	}
	if open.RenderMode != RenderModeWorkerStateXterm {
		t.Fatalf("unexpected render mode: %+v", open)
	}
	mode, err := NewEnvelope(TypeTerminalMode, TerminalMode{
		Mode:       "state-bridge",
		RenderMode: RenderModeWorkerStateXterm,
	})
	if err != nil {
		t.Fatal(err)
	}
	var negotiated TerminalMode
	if err := mode.DecodePayload(&negotiated); err != nil {
		t.Fatal(err)
	}
	if negotiated.RenderMode != RenderModeWorkerStateXterm {
		t.Fatalf("unexpected negotiated render mode: %+v", negotiated)
	}
}

func TestTerminalMousePayloadRoundTrip(t *testing.T) {
	env, err := NewEnvelope(TypeTerminalMouse, TerminalMouse{X: 4, Y: 8, Button: "left", Motion: true, Ctrl: true, Source: "web_state_renderer"})
	if err != nil {
		t.Fatal(err)
	}
	var mouse TerminalMouse
	if err := env.DecodePayload(&mouse); err != nil {
		t.Fatal(err)
	}
	if mouse.X != 4 || mouse.Y != 8 || mouse.Button != "left" || !mouse.Motion || !mouse.Ctrl || mouse.Source != "web_state_renderer" {
		t.Fatalf("unexpected mouse payload: %+v", mouse)
	}
}

func TestTerminalDiffPayloadRoundTrip(t *testing.T) {
	env, err := NewEnvelope(TypeTerminalDiff, TerminalDiff{
		Generation: 3,
		Ops: []TerminalDiffOp{
			{Op: "replace_row", Row: 1, Cells: []TerminalCell{{Text: "x", Fg: "ansi:1", Bg: "ansi:33", UnderlineColor: "#010203"}}},
			{Op: "cursor", Cursor: &TerminalCursor{X: 2, Y: 1, Visible: true}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var diff TerminalDiff
	if err := env.DecodePayload(&diff); err != nil {
		t.Fatal(err)
	}
	cell := diff.Ops[0].Cells[0]
	if diff.Generation != 3 || len(diff.Ops) != 2 || cell.Text != "x" || cell.Fg != "ansi:1" || cell.Bg != "ansi:33" || cell.UnderlineColor != "#010203" || diff.Ops[1].Cursor.X != 2 {
		t.Fatalf("unexpected diff payload: %+v", diff)
	}
}
