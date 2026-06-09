package term

import (
	"io"
	"os"

	"golang.org/x/sys/unix"
)

func MakeRaw(file *os.File) (func(), error) {
	return MakeRawWithTimeout(file, 1, 0)
}

func MakeRawWithTimeout(file *os.File, min, timeout uint8) (func(), error) {
	fd := int(file.Fd())
	old, err := unix.IoctlGetTermios(fd, unix.TIOCGETA)
	if err != nil {
		return func() {}, err
	}
	raw := *old
	raw.Iflag &^= unix.BRKINT | unix.ICRNL | unix.INPCK | unix.ISTRIP | unix.IXON
	raw.Oflag &^= unix.OPOST
	raw.Cflag |= unix.CS8
	raw.Lflag &^= unix.ECHO | unix.ICANON | unix.IEXTEN | unix.ISIG
	raw.Cc[unix.VMIN] = min
	raw.Cc[unix.VTIME] = timeout
	if err := unix.IoctlSetTermios(fd, unix.TIOCSETA, &raw); err != nil {
		return func() {}, err
	}
	return func() {
		_ = unix.IoctlSetTermios(fd, unix.TIOCSETA, old)
	}, nil
}

func Size(file *os.File) (cols int, rows int, err error) {
	ws, err := unix.IoctlGetWinsize(int(file.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		return 120, 36, err
	}
	if ws.Col == 0 || ws.Row == 0 {
		return 120, 36, nil
	}
	return int(ws.Col), int(ws.Row), nil
}

func ResetModes(out io.Writer) {
	if out == nil {
		return
	}
	_, _ = io.WriteString(out,
		"\x1b[?1000l"+ // mouse button events
			"\x1b[?1002l"+ // mouse drag events
			"\x1b[?1003l"+ // all mouse motion events
			"\x1b[?1004l"+ // focus in/out events
			"\x1b[?1006l"+ // SGR mouse mode
			"\x1b[?1015l"+ // urxvt mouse mode
			"\x1b[?2004l"+ // bracketed paste
			"\x1b[?25h"+ // show cursor
			"\x1b[0m", // reset attributes
	)
}

func EnterAlternateScreen(out io.Writer) {
	if out == nil {
		return
	}
	_, _ = io.WriteString(out, "\x1b[?1049h\x1b[H\x1b[2J")
}

func ExitAlternateScreen(out io.Writer) {
	if out == nil {
		return
	}
	_, _ = io.WriteString(out, "\x1b[?1049l")
}
