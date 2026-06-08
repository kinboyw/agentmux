# Roadmap

## Phase 1: Correct Per-Attach PTY

Use one independent tmux client per control attach.

- Worker starts a fresh PTY for each control connection.
- Worker runs `tmux attach-session -t <session>` in that PTY.
- Each attach has a unique `stream_id`.
- Hub routes terminal output by `stream_id`, not only by session id.
- Control input is raw and goes only to that stream's PTY.
- Control detach closes only that stream's tmux client, never the underlying tmux session.
- Control reports terminal size at attach time. Done in CLI control.
- Resize events propagate from control to worker PTY. Done in CLI control.

This keeps tmux as the source of truth and should behave closest to SSH into a
machine and running `tmux attach`.

## Phase 2: Browser Control

Add a browser control surface using xterm.js.

- Hub serves a dashboard.
- Browser uses xterm.js for rendering.
- Browser reports terminal size with fit addon.
- Browser sends raw input and resize events over WebSocket.
- Hub routes streams to worker.

This should become the preferred UI for phones and laptops.

## Phase 3: Headless Terminal State

Add a server-side terminal emulator so reconnecting controls can restore current
screen state instead of only receiving future bytes.

Candidate approaches:

- `@xterm/headless` plus `@xterm/addon-serialize` sidecar.
- Pure Go ANSI parser and cell buffer.
- C/Rust terminal emulator through FFI or sidecar.

Expected benefits:

- clean reconnect
- deterministic screen snapshots
- better multi-control fanout
- session state inspection
- agent status extraction from screen buffer

This is more complex than byte streaming because terminal output is a stateful
protocol, not plain text.
