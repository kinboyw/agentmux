# AgentMux

AgentMux is a tmux-first control plane for long-lived coding-agent sessions.
It is designed as one Go binary with three roles:

- `hub`: public HTTPS/WSS entrypoint and routing layer.
- `worker`: outbound connector that manages local tmux sessions.
- `control`: CLI client for listing, creating, attaching, and sending input.

The agent is intentionally unaware of AgentMux. Codex, Claude, Gemini, OpenCode,
or a shell simply run inside tmux. Worker observes and controls tmux.

## Quick Start

Fast local verification:

```bash
./scripts/dev-tmux.sh
```

This starts one tmux session named `agentmux-dev` with one window and three panes:

- `hub`
- `worker`
- `control`

The default port is `8081` and the default token is `dev-token`.

Terminal 1:

```bash
go run ./cmd/agentmux hub --addr 127.0.0.1:8080 --token dev-token
```

Terminal 2:

```bash
go run ./cmd/agentmux worker --hub ws://127.0.0.1:8080 --token dev-token --name local
```

Terminal 3:

```bash
go run ./cmd/agentmux control list --hub http://127.0.0.1:8080 --token dev-token
go run ./cmd/agentmux control create --hub http://127.0.0.1:8080 --token dev-token --worker local --name demo --cwd "$PWD" --command bash
go run ./cmd/agentmux control attach --hub ws://127.0.0.1:8080 --token dev-token --session local/demo
```

In `control attach`, press `Ctrl-]` to detach from the local control session.
This only closes the local control connection; the worker-side session remains
alive. `Ctrl-C` is forwarded to the remote agent.

## Documents

- [Design](docs/DESIGN.md)
- [API](docs/API.md)
- [Roadmap](docs/ROADMAP.md)
