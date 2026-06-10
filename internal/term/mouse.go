package term

import "io"

func EnableMouseCellMotion(out io.Writer) {
	if out == nil {
		return
	}
	_, _ = io.WriteString(out, "\x1b[?1002h\x1b[?1006h")
}
