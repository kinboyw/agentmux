# AgentMux local patch

This directory is a local fork of `github.com/charmbracelet/x/vt` at
`v0.0.0-20260305213658-fe36e8c10185` (MIT licensed; see `LICENSE`).

## Why it is pinned locally

AgentMux drains terminal responses in a background goroutine. The upstream
`Emulator` accessed its `closed` flag from `Read`, `Write`, and `Close` without
synchronization. Closing a terminal while the response drain was blocked in
`Read` therefore triggered a Go data race.

The patch changes that flag to `atomic.Bool`, preserving the upstream API while
making concurrent `Read`/`Close` state checks race-safe. AgentMux uses
`vt.SafeEmulator` for all screen-state operations and waits for its response
reader to exit during `terminalview.View.Close`.

Remove this fork and the `replace` directive in the repository root `go.mod`
once upstream provides an equivalent fix. Keep this note and the upstream MIT
license whenever the fork is refreshed.
