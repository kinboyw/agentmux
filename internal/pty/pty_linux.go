package pty

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"
)

const (
	tiocgptn   = 0x80045430
	tiocsptlck = 0x40045431
	tiocswinsz = 0x5414
)

type Terminal struct {
	master *os.File
	cmd    *exec.Cmd
}

func StartTmuxAttach(ctx context.Context, session string, cols int, rows int) (*Terminal, error) {
	master, slave, err := openPTY(cols, rows)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "tmux", "attach-session", "-t", session)
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	cmd.Env = cleanEnv(os.Environ())
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
		Ctty:    0,
	}
	if err := cmd.Start(); err != nil {
		_ = master.Close()
		_ = slave.Close()
		return nil, err
	}
	_ = slave.Close()
	return &Terminal{master: master, cmd: cmd}, nil
}

func (t *Terminal) Read(p []byte) (int, error) {
	return t.master.Read(p)
}

func (t *Terminal) Write(p []byte) (int, error) {
	return t.master.Write(p)
}

func (t *Terminal) Resize(cols int, rows int) error {
	return setWinsize(t.master.Fd(), cols, rows)
}

func (t *Terminal) Close() error {
	_ = t.master.Close()
	return nil
}

func openPTY(cols int, rows int) (*os.File, *os.File, error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, err
	}
	unlock := 0
	if err := ioctl(master.Fd(), tiocsptlck, uintptr(unsafe.Pointer(&unlock))); err != nil {
		_ = master.Close()
		return nil, nil, err
	}
	var number uint32
	if err := ioctl(master.Fd(), tiocgptn, uintptr(unsafe.Pointer(&number))); err != nil {
		_ = master.Close()
		return nil, nil, err
	}
	if err := setWinsize(master.Fd(), cols, rows); err != nil {
		_ = master.Close()
		return nil, nil, err
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		_ = master.Close()
		return nil, nil, err
	}
	return master, slave, nil
}

func setWinsize(fd uintptr, cols int, rows int) error {
	if cols <= 0 {
		cols = 120
	}
	if rows <= 0 {
		rows = 36
	}
	ws := struct {
		row    uint16
		col    uint16
		xpixel uint16
		ypixel uint16
	}{row: uint16(rows), col: uint16(cols)}
	return ioctl(fd, tiocswinsz, uintptr(unsafe.Pointer(&ws)))
}

func ioctl(fd uintptr, request uintptr, arg uintptr) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, request, arg)
	if errno != 0 {
		return errno
	}
	return nil
}

func cleanEnv(values []string) []string {
	cleaned := make([]string, 0, len(values)+2)
	hasTerm := false
	for _, value := range values {
		switch {
		case len(value) >= 5 && value[:5] == "TMUX=":
			continue
		case len(value) >= 5 && value[:5] == "TERM=":
			hasTerm = true
			cleaned = append(cleaned, "TERM=xterm-256color")
		default:
			cleaned = append(cleaned, value)
		}
	}
	if !hasTerm {
		cleaned = append(cleaned, "TERM=xterm-256color")
	}
	cleaned = append(cleaned, "COLORTERM=truecolor")
	return cleaned
}
