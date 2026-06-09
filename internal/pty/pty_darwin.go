package pty

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

type Terminal struct {
	master *os.File
	cmd    *exec.Cmd
}

func StartTmuxAttach(ctx context.Context, session string, cols int, rows int) (*Terminal, error) {
	return StartCommand(ctx, "tmux", []string{"attach-session", "-t", session}, "", cols, rows)
}

func StartCommand(ctx context.Context, name string, args []string, cwd string, cols int, rows int) (*Terminal, error) {
	master, slave, err := openPTY(cols, rows)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, name, args...)
	if cwd != "" {
		abs, err := filepath.Abs(cwd)
		if err != nil {
			_ = master.Close()
			_ = slave.Close()
			return nil, err
		}
		cmd.Dir = abs
	}
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

func (t *Terminal) Wait() error {
	if t.cmd == nil {
		return nil
	}
	return t.cmd.Wait()
}

func (t *Terminal) Master() *os.File {
	return t.master
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

func (t *Terminal) Kill() error {
	if t.cmd == nil || t.cmd.Process == nil {
		return nil
	}
	if err := t.cmd.Process.Kill(); err != nil && err != os.ErrProcessDone {
		return err
	}
	return nil
}

func (t *Terminal) CopyOutput(ctx context.Context, writers func([]byte)) error {
	buffer := make([]byte, 8192)
	for {
		n, err := t.Read(buffer)
		if n > 0 {
			chunk := append([]byte(nil), buffer[:n]...)
			writers(chunk)
		}
		if err != nil {
			if err != io.EOF && ctx.Err() == nil {
				return err
			}
			return nil
		}
	}
}

func openPTY(cols int, rows int) (*os.File, *os.File, error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, err
	}
	if err := ioctl(master.Fd(), unix.TIOCPTYGRANT, 0); err != nil {
		_ = master.Close()
		return nil, nil, err
	}
	if err := ioctl(master.Fd(), unix.TIOCPTYUNLK, 0); err != nil {
		_ = master.Close()
		return nil, nil, err
	}
	name := make([]byte, 128)
	if err := ioctl(master.Fd(), unix.TIOCPTYGNAME, uintptr(unsafe.Pointer(&name[0]))); err != nil {
		_ = master.Close()
		return nil, nil, err
	}
	if err := setWinsize(master.Fd(), cols, rows); err != nil {
		_ = master.Close()
		return nil, nil, err
	}
	slave, err := os.OpenFile(cString(name), os.O_RDWR|unix.O_NOCTTY, 0)
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
	return unix.IoctlSetWinsize(int(fd), unix.TIOCSWINSZ, &unix.Winsize{
		Row: uint16(rows),
		Col: uint16(cols),
	})
}

func ioctl(fd uintptr, request uint, arg uintptr) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(request), arg)
	if errno != 0 {
		return errno
	}
	return nil
}

func cString(data []byte) string {
	if index := strings.IndexByte(string(data), 0); index >= 0 {
		return string(data[:index])
	}
	return string(data)
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
