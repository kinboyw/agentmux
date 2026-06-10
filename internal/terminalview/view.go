package terminalview

import (
	"io"
	"strings"

	"github.com/charmbracelet/x/vt"
)

const defaultScrollback = 2000

type View struct {
	width  int
	height int
	term   *vt.Emulator
}

func New(width, height int) *View {
	width, height = normalizeSize(width, height)
	term := vt.NewEmulator(width, height)
	term.SetScrollbackSize(defaultScrollback)
	go drainTerminalResponses(term)
	return &View{width: width, height: height, term: term}
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

func normalizeSize(width, height int) (int, int) {
	if width < 1 {
		width = 80
	}
	if height < 1 {
		height = 24
	}
	return width, height
}
