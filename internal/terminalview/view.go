package terminalview

import (
	"image/color"
	"io"
	"strconv"
	"strings"
	"sync"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
)

const defaultScrollback = 1

type View struct {
	mu        sync.RWMutex
	mouseMu   sync.RWMutex
	width     int
	height    int
	term      *vt.SafeEmulator
	drainDone chan struct{}
	closed    bool

	mouseModes map[int]bool
	mouseSGR   bool
}

type CellSnapshot struct {
	Cols   int
	Rows   int
	Cursor Cursor
	Lines  [][]Cell
}

type Cursor struct {
	X       int
	Y       int
	Visible bool
}

type Cell struct {
	Text           string
	Width          int
	Fg             string
	Bg             string
	Bold           bool
	Faint          bool
	Italic         bool
	Blink          bool
	Reverse        bool
	Conceal        bool
	Strikethrough  bool
	Underline      string
	UnderlineColor string
	Link           string
}

func New(width, height int) *View {
	view := &View{}
	view.initLocked(width, height)
	return view
}

func (v *View) initLocked(width, height int) {
	width, height = normalizeSize(width, height)
	term := vt.NewSafeEmulator(width, height)
	term.SetScrollbackSize(defaultScrollback)
	term.SetCallbacks(vt.Callbacks{
		EnableMode:  v.enableMode,
		DisableMode: v.disableMode,
	})
	v.width = width
	v.height = height
	v.term = term
	v.closed = false
	v.mouseMu.Lock()
	v.mouseModes = map[int]bool{}
	v.mouseSGR = false
	v.mouseMu.Unlock()
	v.drainDone = make(chan struct{})
	go func() {
		drainTerminalResponses(term)
		close(v.drainDone)
	}()
}

func drainTerminalResponses(term *vt.SafeEmulator) {
	_, _ = io.Copy(io.Discard, term)
}

func (v *View) Resize(width, height int) {
	width, height = normalizeSize(width, height)
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return
	}
	if v.term == nil {
		v.initLocked(width, height)
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
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return
	}
	if v.term == nil {
		v.initLocked(v.width, v.height)
	}
	_, _ = v.term.Write(data)
}

func (v *View) Render() string {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.term == nil || v.closed {
		return ""
	}
	return strings.TrimRight(v.term.Render(), "\n")
}

func (v *View) Screen() string {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.term == nil || v.closed {
		return ""
	}
	return v.term.Render()
}

func (v *View) Cursor() (int, int, bool) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.term == nil || v.closed {
		return 0, 0, false
	}
	pos := v.term.CursorPosition()
	return pos.X, pos.Y, true
}

func (v *View) Cells() CellSnapshot {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.term == nil || v.closed {
		return CellSnapshot{}
	}
	lines := make([][]Cell, v.height)
	for y := 0; y < v.height; y++ {
		line := make([]Cell, v.width)
		for x := 0; x < v.width; x++ {
			line[x] = snapshotCell(v.term.CellAt(x, y))
		}
		lines[y] = line
	}
	pos := v.term.CursorPosition()
	return CellSnapshot{
		Cols:   v.width,
		Rows:   v.height,
		Cursor: Cursor{X: pos.X, Y: pos.Y, Visible: true},
		Lines:  lines,
	}
}

func (v *View) ANSI() string {
	return SnapshotANSI(v.Cells())
}

func (v *View) Close() {
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		return
	}
	v.closed = true
	term := v.term
	drainDone := v.drainDone
	v.mu.Unlock()
	if term != nil {
		_ = term.Close()
	}
	if drainDone != nil {
		<-drainDone
	}
}

func SnapshotANSI(snapshot CellSnapshot) string {
	var out strings.Builder
	out.WriteString("\x1b[0m\x1b[2J")
	currentStyle := ""
	for y, line := range snapshot.Lines {
		writeCursorMove(&out, y+1, 1)
		currentStyle = ""
		for x := 0; x < len(line); x++ {
			cell := line[x]
			if cell.Width == 0 {
				continue
			}
			style := cellSGR(cell)
			if style != currentStyle {
				out.WriteString(style)
				currentStyle = style
			}
			text := cell.Text
			if text == "" || cell.Conceal {
				text = " "
			}
			out.WriteString(text)
			if cell.Width > 1 {
				x += cell.Width - 1
			}
		}
		if currentStyle != "" {
			out.WriteString("\x1b[0m")
			currentStyle = ""
		}
	}
	if snapshot.Cursor.Visible {
		out.WriteString("\x1b[?25h")
		writeCursorMove(&out, snapshot.Cursor.Y+1, snapshot.Cursor.X+1)
	} else {
		out.WriteString("\x1b[?25l")
	}
	return out.String()
}

func writeCursorMove(out *strings.Builder, row, col int) {
	if row < 1 {
		row = 1
	}
	if col < 1 {
		col = 1
	}
	out.WriteString("\x1b[")
	out.WriteString(strconv.Itoa(row))
	out.WriteByte(';')
	out.WriteString(strconv.Itoa(col))
	out.WriteByte('H')
}

func cellSGR(cell Cell) string {
	params := []string{"0"}
	if cell.Bold {
		params = append(params, "1")
	}
	if cell.Faint {
		params = append(params, "2")
	}
	if cell.Italic {
		params = append(params, "3")
	}
	if cell.Underline != "" {
		params = append(params, underlineSGR(cell.Underline))
	}
	if cell.Blink {
		params = append(params, "5")
	}
	if cell.Reverse {
		params = append(params, "7")
	}
	if cell.Strikethrough {
		params = append(params, "9")
	}
	if fg := colorSGR(cell.Fg, false); fg != "" {
		params = append(params, fg)
	}
	if bg := colorSGR(cell.Bg, true); bg != "" {
		params = append(params, bg)
	}
	if underline := underlineColorSGR(cell.UnderlineColor); underline != "" {
		params = append(params, underline)
	}
	if len(params) == 1 {
		return ""
	}
	return "\x1b[" + strings.Join(params, ";") + "m"
}

func underlineSGR(style string) string {
	switch style {
	case "double":
		return "4:2"
	case "curly":
		return "4:3"
	case "dotted":
		return "4:4"
	case "dashed":
		return "4:5"
	default:
		return "4"
	}
}

func colorSGR(ref string, background bool) string {
	if ref == "" {
		return ""
	}
	if strings.HasPrefix(ref, "ansi:") {
		index, err := strconv.Atoi(strings.TrimPrefix(ref, "ansi:"))
		if err != nil || index < 0 || index > 255 {
			return ""
		}
		if index < 8 {
			if background {
				return strconv.Itoa(40 + index)
			}
			return strconv.Itoa(30 + index)
		}
		if index < 16 {
			if background {
				return strconv.Itoa(100 + index - 8)
			}
			return strconv.Itoa(90 + index - 8)
		}
		if background {
			return "48;5;" + strconv.Itoa(index)
		}
		return "38;5;" + strconv.Itoa(index)
	}
	if red, green, blue, ok := parseHexColor(ref); ok {
		prefix := "38"
		if background {
			prefix = "48"
		}
		return prefix + ";2;" + strconv.Itoa(red) + ";" + strconv.Itoa(green) + ";" + strconv.Itoa(blue)
	}
	return ""
}

func underlineColorSGR(ref string) string {
	if ref == "" {
		return ""
	}
	if strings.HasPrefix(ref, "ansi:") {
		index, err := strconv.Atoi(strings.TrimPrefix(ref, "ansi:"))
		if err != nil || index < 0 || index > 255 {
			return ""
		}
		return "58;5;" + strconv.Itoa(index)
	}
	if red, green, blue, ok := parseHexColor(ref); ok {
		return "58;2;" + strconv.Itoa(red) + ";" + strconv.Itoa(green) + ";" + strconv.Itoa(blue)
	}
	return ""
}

func parseHexColor(ref string) (int, int, int, bool) {
	if len(ref) != 7 || ref[0] != '#' {
		return 0, 0, 0, false
	}
	value, err := strconv.ParseUint(ref[1:], 16, 32)
	if err != nil {
		return 0, 0, 0, false
	}
	return int(value >> 16 & 0xff), int(value >> 8 & 0xff), int(value & 0xff), true
}

func snapshotCell(cell *uv.Cell) Cell {
	if cell == nil || cell.IsZero() {
		return Cell{Text: " ", Width: 1}
	}
	out := Cell{Text: cell.Content, Width: cell.Width}
	if out.Text == "" {
		out.Text = " "
	}
	if out.Width <= 0 {
		out.Width = 1
	}
	style := cell.Style
	out.Fg = terminalColorRef(style.Fg)
	out.Bg = terminalColorRef(style.Bg)
	out.UnderlineColor = terminalColorRef(style.UnderlineColor)
	out.Bold = style.Attrs&uv.AttrBold != 0
	out.Faint = style.Attrs&uv.AttrFaint != 0
	out.Italic = style.Attrs&uv.AttrItalic != 0
	out.Blink = style.Attrs&uv.AttrBlink != 0 || style.Attrs&uv.AttrRapidBlink != 0
	out.Reverse = style.Attrs&uv.AttrReverse != 0
	out.Conceal = style.Attrs&uv.AttrConceal != 0
	out.Strikethrough = style.Attrs&uv.AttrStrikethrough != 0
	switch style.Underline {
	case uv.UnderlineSingle:
		out.Underline = "single"
	case uv.UnderlineDouble:
		out.Underline = "double"
	case uv.UnderlineCurly:
		out.Underline = "curly"
	case uv.UnderlineDotted:
		out.Underline = "dotted"
	case uv.UnderlineDashed:
		out.Underline = "dashed"
	}
	if !cell.Link.IsZero() {
		out.Link = cell.Link.URL
	}
	return out
}

func terminalColorRef(c color.Color) string {
	if c == nil {
		return ""
	}
	switch value := c.(type) {
	case ansi.BasicColor:
		return "ansi:" + strconv.Itoa(int(value))
	case ansi.IndexedColor:
		return "ansi:" + strconv.Itoa(int(value))
	case ansi.TrueColor:
		r, g, b, _ := value.RGBA()
		return rgbHex(r, g, b)
	case ansi.RGBColor:
		r, g, b, _ := value.RGBA()
		return rgbHex(r, g, b)
	default:
		r, g, b, _ := c.RGBA()
		return rgbHex(r, g, b)
	}
}

func rgbHex(r, g, b uint32) string {
	return "#" + hexByte(byte(r>>8)) + hexByte(byte(g>>8)) + hexByte(byte(b>>8))
}

func hexByte(value byte) string {
	const digits = "0123456789abcdef"
	return string([]byte{digits[value>>4], digits[value&0x0f]})
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
	v.mu.RLock()
	if v.closed {
		v.mu.RUnlock()
		return ""
	}
	v.mouseMu.RLock()
	mode := v.mouseModeLocked()
	mouseSGR := v.mouseSGR
	v.mouseMu.RUnlock()
	v.mu.RUnlock()
	if mode == 0 || !mouseEventAllowed(mode, event) {
		return ""
	}
	button := ansi.EncodeMouseButton(event.Button, event.Motion, event.Shift, event.Alt, event.Ctrl)
	if button == 0xff {
		return ""
	}
	if mouseSGR {
		return ansi.MouseSgr(button, event.X, event.Y, event.Release)
	}
	return ansi.MouseX10(button, event.X, event.Y)
}

func (v *View) enableMode(mode ansi.Mode) {
	v.mouseMu.Lock()
	defer v.mouseMu.Unlock()
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
	v.mouseMu.Lock()
	defer v.mouseMu.Unlock()
	switch mode.Mode() {
	case 9, 1000, 1001, 1002, 1003:
		delete(v.mouseModes, mode.Mode())
	case 1006:
		v.mouseSGR = false
	}
}

func (v *View) mouseModeLocked() int {
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
