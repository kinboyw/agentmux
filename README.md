<p align="center">
  <img src="docs/assets/brand/agentmux-logo.svg" alt="AgentMux" width="520">
</p>

# AgentMux

[English](README.md) | [中文](README.zh-CN.md)

AgentMux is a tmux-first control plane for long-lived coding-agent sessions.
It is designed as one Go binary with three roles:

- `hub`: public HTTPS/WSS entrypoint and routing layer.
- `worker`: outbound connector that manages local tmux sessions.
- `control`: CLI client for listing, creating, attaching, and sending input.
  The product control surface is the hub-hosted browser page at `/control`.

The agent is intentionally unaware of AgentMux. Codex, Claude, Gemini, OpenCode,
or a shell simply run inside tmux. Worker observes and controls tmux.

![AgentMux system architecture](docs/assets/visuals/agentmux-1-system-architecture-v1.png)

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

Browser control:

```text
http://127.0.0.1:8081/control?token=dev-token
```

Project Go version is managed by mise:

```bash
mise install
mise exec -- go test ./...
```

The browser UI uses xterm.js for the terminal area and keeps session identity,
connection state, and detach controls outside the remote terminal buffer.
The modern Web control source lives in `web/control` and is embedded into the
Hub from `internal/hub/webdist`.

The Web control also has a development-stage registered-user flow. Register or
sign in from the sidebar to receive a scoped `amx_cred_...` control credential.
Current credentials and users are in-memory; restarting the Hub clears them.
GitHub and Google buttons are wired to OAuth provider endpoints, but providers
are not configured yet.

![AgentMux Web Control workspace](docs/assets/visuals/agentmux-4-web-control-workspace-v1.png)

Persistent Hub mode uses SQLite:

```bash
agentmux hub \
  --addr 0.0.0.0:8080 \
  --data ./agentmux.db \
  --public-url https://hub.example.com
```

`--data` persists signals, credentials, and registered users. Online workers,
active WebSocket streams, and live session snapshots are still runtime state and
are rebuilt as workers reconnect. `--public-url` controls generated worker,
control, and landing-page commands when the Hub sits behind Cloudflare Tunnel,
Nginx, Caddy, or another reverse proxy.

Cloudflare Tunnel example:

```bash
agentmux hub --addr 127.0.0.1:8080 --data ./agentmux.db --public-url https://hub.example.com
cloudflared tunnel --url http://127.0.0.1:8080
```

Cloudflare terminates HTTPS and forwards WebSocket upgrades to the Go Hub. The
Hub generates `wss://...` worker URLs automatically when the public URL is
HTTPS.

![AgentMux Cloudflare deployment](docs/assets/visuals/agentmux-3-cloudflare-deployment-v1.png)

See [Deployment](docs/DEPLOYMENT.md) for Docker Compose, systemd, Cloudflare
Tunnel, and release install script details.

Signal-based onboarding:

1. Open `http://127.0.0.1:8081/`.
2. Generate a signal.
3. Run the generated worker command.
4. Open the generated Web Control URL.

Signals look like `amx_sig_...` and are exchanged for scoped `amx_cred_...`
credentials before normal API or WebSocket access.

![AgentMux signal onboarding](docs/assets/visuals/agentmux-2-zero-tier-style-onboarding-v1.png)

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

## Build And Release

CI runs Go tests, builds the Web Control, and builds the CLI binary through
GitHub Actions. Tag pushes like `v0.1.0` trigger cross-platform release assets.

Local release-style build:

```bash
cd web/control
npm ci
npm run build
cd ../..
rm -rf internal/hub/webdist
mkdir -p internal/hub/webdist
cp -a web/control/dist/. internal/hub/webdist/
mise exec -- go build -o dist/agentmux ./cmd/agentmux
```

## Documents

- [Design](docs/DESIGN.md)
- [Product Architecture](docs/PRODUCT_ARCHITECTURE.md)
- [API](docs/API.md)
- [Deployment](docs/DEPLOYMENT.md)
- [Roadmap](docs/ROADMAP.md)
- [Visual Prompt Library](docs/visual-prompts/README.md)
- [GitHub Repository Metadata](docs/GITHUB.md)
