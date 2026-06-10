package terminalview

import (
	"io"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
)

const defaultScrollback = 2000

type View struct {
	width      int
	height     int
	term       *vt.Emulator
	mouseModes map[int]bool
	mouseSGR   bool
}

func New(width, height int) *View {
	width, height = normalizeSize(width, height)
	term := vt.NewEmulator(width, height)
	term.SetScrollbackSize(defaultScrollback)
	view := &View{width: width, height: height, term: term, mouseModes: map[int]bool{}}
	term.SetCallbacks(vt.Callbacks{
		EnableMode:  view.enableMode,
		DisableMode: view.disableMode,
	})
	go drainTerminalResponses(term)
	return view
}

func drainTerminalResponses(term *vt.Emulator) {
	_, _ = io.Copy(io.Discard, term)
}

func (v *View) Resize(width, height int) {
	width, height = normalizeSize(width, height)
	if v.term == nil {
		*v = *New(width, height)
		return
	}
	if v.width == width && v.height == height {
		return
	}
	v.width = width
	v.height = height
	v.term.Resize(width, height)
}

func (v *View) Write(data []byte) {
	if len(data) == 0 {
		return
	}
	if v.term == nil {
		*v = *New(v.width, v.height)
	}
	_, _ = v.term.Write(data)
}

func (v *View) Render() string {
	if v.term == nil {
		return ""
	}
	return strings.TrimRight(v.term.Render(), "\n")
}

func (v *View) Screen() string {
	if v.term == nil {
		return ""
	}
	return v.term.Render()
}

func (v *View) Cursor() (int, int, bool) {
	if v.term == nil {
		return 0, 0, false
	}
	pos := v.term.CursorPosition()
	return pos.X, pos.Y, true
}

func (v *View) Close() {
	if v.term != nil {
		_ = v.term.Close()
	}
}

type MouseButton = ansi.MouseButton

const (
	MouseNone       = ansi.MouseNone
	MouseLeft       = ansi.MouseLeft
	MouseMiddle     = ansi.MouseMiddle
	MouseRight      = ansi.MouseRight
	MouseWheelUp    = ansi.MouseWheelUp
	MouseWheelDown  = ansi.MouseWheelDown
	MouseWheelLeft  = ansi.MouseWheelLeft
	MouseWheelRight = ansi.MouseWheelRight
	MouseBackward   = ansi.MouseBackward
	MouseForward    = ansi.MouseForward
)

type MouseEvent struct {
	X       int
	Y       int
	Button  MouseButton
	Motion  bool
	Release bool
	Shift   bool
	Alt     bool
	Ctrl    bool
}

func (v *View) MouseInput(event MouseEvent) string {
	if v == nil || event.X < 0 || event.Y < 0 {
		return ""
	}
	mode := v.mouseMode()
	if mode == 0 || !mouseEventAllowed(mode, event) {
		return ""
	}
	button := ansi.EncodeMouseButton(event.Button, event.Motion, event.Shift, event.Alt, event.Ctrl)
	if button == 0xff {
		return ""
	}
	if v.mouseSGR {
		return ansi.MouseSgr(button, event.X, event.Y, event.Release)
	}
	return ansi.MouseX10(button, event.X, event.Y)
}

func (v *View) enableMode(mode ansi.Mode) {
	switch mode.Mode() {
	case 9, 1000, 1001, 1002, 1003:
		if v.mouseModes == nil {
			v.mouseModes = map[int]bool{}
		}
		v.mouseModes[mode.Mode()] = true
	case 1006:
		v.mouseSGR = true
	}
}

func (v *View) disableMode(mode ansi.Mode) {
	switch mode.Mode() {
	case 9, 1000, 1001, 1002, 1003:
		delete(v.mouseModes, mode.Mode())
	case 1006:
		v.mouseSGR = false
	}
}

func (v *View) mouseMode() int {
	if v == nil {
		return 0
	}
	mode := 0
	for _, candidate := range []int{9, 1000, 1001, 1002, 1003} {
		if v.mouseModes[candidate] {
			mode = candidate
		}
	}
	return mode
}

func mouseEventAllowed(mode int, event MouseEvent) bool {
	if event.Motion {
		switch mode {
		case 1002:
			return event.Button != MouseNone
		case 1003:
			return true
		default:
			return false
		}
	}
	if event.Release {
		return mode != 9
	}
	return true
}

func normalizeSize(width, height int) (int, int) {
	if width < 1 {
		width = 80
	}
	if height < 1 {
		height = 24
	}
	return width, height
}
