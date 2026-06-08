package control

import (
	"context"
	"io"
	"os"
	"os/signal"
	"syscall"

	"private/agentmux/internal/protocol"
	"private/agentmux/internal/term"
)

func (c Client) Attach(ctx context.Context, sessionID string, in io.Reader, out io.Writer) error {
	cols, rows := terminalSize(in)
	stream, err := c.OpenStream(ctx, sessionID, protocol.TerminalSize{Cols: cols, Rows: rows})
	if err != nil {
		return err
	}
	defer stream.Close()

	term.EnterAlternateScreen(out)
	defer func() {
		term.ResetModes(out)
		term.ExitAlternateScreen(out)
	}()

	errc := make(chan error, 2)
	go func() {
		for {
			event, err := stream.ReadEvent()
			if err != nil {
				errc <- err
				return
			}
			if event.Err != nil {
				errc <- event.Err
				return
			}
			if len(event.Data) > 0 {
				_, _ = out.Write(event.Data)
			}
		}
	}()
	if file, ok := in.(*os.File); ok {
		if restore, err := term.MakeRaw(file); err == nil {
			defer restore()
		}
		go watchResize(ctx, file, stream, errc)
	}
	go func() {
		buffer := make([]byte, 1024)
		for {
			n, err := in.Read(buffer)
			if n > 0 {
				if containsDetachKey(buffer[:n]) {
					errc <- nil
					return
				}
				if err := stream.Input(string(buffer[:n])); err != nil {
					errc <- err
					return
				}
			}
			if err != nil {
				errc <- err
				return
			}
		}
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errc:
		return err
	}
}

func terminalSize(in io.Reader) (int, int) {
	file, ok := in.(*os.File)
	if !ok {
		return 120, 36
	}
	cols, rows, err := term.Size(file)
	if err != nil {
		return 120, 36
	}
	return cols, rows
}

func watchResize(ctx context.Context, file *os.File, stream *Stream, errc chan<- error) {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGWINCH)
	defer signal.Stop(signals)
	for {
		select {
		case <-ctx.Done():
			return
		case <-signals:
			cols, rows, err := term.Size(file)
			if err != nil {
				continue
			}
			if err := stream.Resize(protocol.TerminalSize{Cols: cols, Rows: rows}); err != nil {
				errc <- err
				return
			}
		}
	}
}

func containsDetachKey(data []byte) bool {
	for _, b := range data {
		if b == 0x1d {
			return true
		}
	}
	return false
}
