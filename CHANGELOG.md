# Changelog

## v0.0.5

Worker-side terminal state, Web Control mobile polish, and release packaging updates.

- Added worker-side terminal snapshots, row-level terminal diffs, history pages,
  state reset, mouse, and size-control protocol messages.
- Added Web Control support for worker-state xterm attach, worker-side history,
  state diagnostics, and remote terminal size sync/reset.
- Fixed Web Control so Worker history does not open automatically on initial
  remote session attach.
- Improved mobile Web Control with compact overview metrics, folded terminal
  actions, Worker Ops menus, and session/window/pane navigation refinements.
- Added session and pane favorites backed by browser local storage.
- Expanded release packaging for Worker, Control, TUI, and Hub assets with
  checksums.
- Updated terminal state, API, usage, architecture, roadmap, deployment, and
  update strategy documentation.

See [docs/releases/v0.0.5.md](docs/releases/v0.0.5.md) for the full release notes.

## v0.0.2

Worker onboarding and CLI usability release.

- Added local credential caching for Worker and Control signal exchange.
- Added `agentmux worker join`, `run`, `start`, `stop`, `status`, and `logs`.
- Added background Worker service support using `systemd --user`, macOS `launchd`, or a fallback user process.
- Added the `agentmux-tui` entrypoint for joining and launching the terminal Control app.
- Updated `/install.sh` to join Workers as background services and install an `agentmux-tui` symlink.
- Added tmux availability checks with actionable installation guidance.
- Fixed WSS dialing so public HTTPS hubs without explicit ports use `443` instead of `80`.

See [docs/releases/v0.0.2.md](docs/releases/v0.0.2.md) for the full release notes.

## v0.0.1

Initial public prototype release.

See [docs/releases/v0.0.1.md](docs/releases/v0.0.1.md) for the full release notes.
