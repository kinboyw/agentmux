package term

import (
	"io"
	"os"
	"syscall"
	"unsafe"
)

const (
	tcgets     = 0x5401
	tcsets     = 0x5402
	tiocgwinsz = 0x5413
)

func MakeRaw(file *os.File) (func(), error) {
	fd := file.Fd()
	var old syscall.Termios
	if err := ioctl(fd, tcgets, uintptr(unsafe.Pointer(&old))); err != nil {
		return func() {}, err
	}
	raw := old
	raw.Iflag &^= syscall.BRKINT | syscall.ICRNL | syscall.INPCK | syscall.ISTRIP | syscall.IXON
	raw.Oflag &^= syscall.OPOST
	raw.Cflag |= syscall.CS8
	raw.Lflag &^= syscall.ECHO | syscall.ICANON | syscall.IEXTEN | syscall.ISIG
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0
	if err := ioctl(fd, tcsets, uintptr(unsafe.Pointer(&raw))); err != nil {
		return func() {}, err
	}
	return func() {
		_ = ioctl(fd, tcsets, uintptr(unsafe.Pointer(&old)))
	}, nil
}

func ioctl(fd uintptr, request uintptr, arg uintptr) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, request, arg)
	if errno != 0 {
		return errno
	}
	return nil
}

func Size(file *os.File) (cols int, rows int, err error) {
	ws := struct {
		row    uint16
		col    uint16
		xpixel uint16
		ypixel uint16
	}{}
	if err := ioctl(file.Fd(), tiocgwinsz, uintptr(unsafe.Pointer(&ws))); err != nil {
		return 120, 36, err
	}
	if ws.col == 0 || ws.row == 0 {
		return 120, 36, nil
	}
	return int(ws.col), int(ws.row), nil
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
			"\x1b[?1049l"+ // alternate screen
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
