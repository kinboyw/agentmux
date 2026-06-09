# Changelog

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
