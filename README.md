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

If you are already viewing an AgentMux Hub landing page, use that Hub directly:

1. Click `Generate join signal`.
2. Run the generated Worker command on the machine that owns your tmux sessions.
3. Open the generated Web Control URL from your browser.

To self-host a Hub, run the published container image:

```bash
docker run -d --name agentmux --restart unless-stopped -p 8081:8081 ghcr.io/kinboyw/agentmux:latest
```

The container stores Hub state under `/var/lib/agentmux` by default. Mount that
directory only when you want to persist the embedded database outside the
container.

If the self-hosted Hub does not already have a public URL, put it behind
Cloudflare Tunnel, ngrok, or another reverse proxy first, then open the public
URL and generate Worker and Control commands from that page.

For local source verification:

```bash
./scripts/dev-tmux.sh
```

This starts Hub, Worker, and Control panes in one tmux session. The default port
is `8081` and the default token is `dev-token`.

Project Go version is managed by mise:

```bash
mise install
mise exec -- go test ./...
```

The browser UI uses xterm.js for the terminal area and keeps session identity,
connection state, and detach controls outside the remote terminal buffer.
The modern Web control source lives in `web/control` and is embedded into the
Hub from `internal/hub/webdist`.

The Web control also includes a registered-user flow. Register or sign in from
the sidebar to receive a scoped `amx_cred_...` control credential. When Hub runs
with `--data`, users, signals, and credentials are persisted in SQLite. Without
`--data`, Hub uses the development in-memory store. GitHub and Google buttons
are wired to OAuth provider endpoints, but providers are not configured yet.

![AgentMux Web Control workspace](docs/assets/visuals/agentmux-4-web-control-workspace-v1.png)

Persistent Hub mode uses SQLite:

```bash
agentmux hub \
  --addr 0.0.0.0:8080 \
  --data ./agentmux.db \
  --public-url https://hub.example.com
```

`--data` persists signals, credentials, and registered users. Online workers,
active WebSocket streams, and live session snapshots are runtime state and are
rebuilt as workers reconnect. `--public-url` is the external URL that Hub uses
when it generates Worker, Control, and landing-page commands.

Cloudflare named tunnel example:

```bash
agentmux hub --addr 127.0.0.1:8080 --data ./agentmux.db --public-url https://hub.example.com
cloudflared tunnel run agentmux
```

Cloudflare terminates HTTPS and forwards WebSocket upgrades to the Go Hub. The
Hub generates `wss://...` worker URLs automatically when the public URL is
HTTPS.

For a quick `trycloudflare.com` tunnel, run `cloudflared tunnel --url ...`
first, copy the printed URL, then restart Hub with that value as `--public-url`.

![AgentMux Cloudflare deployment](docs/assets/visuals/agentmux-3-cloudflare-deployment-v1.png)

See [Deployment](docs/DEPLOYMENT.md) for Docker Compose, systemd, Cloudflare
Tunnel, and release install script details.

Docker images are published to GitHub Container Registry on release tags:

```bash
docker pull ghcr.io/kinboyw/agentmux:latest
```

Signal-based onboarding:

1. Open `http://127.0.0.1:8081/`.
2. Generate a signal.
3. Run the generated worker command.
4. Open the generated Web Control URL.

Signals look like `amx_sig_...` and are exchanged for scoped `amx_cred_...`
credentials before normal API or WebSocket access.

![AgentMux signal onboarding](docs/assets/visuals/agentmux-2-zero-tier-style-onboarding-v1.png)

Manual source-development commands:

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
- [Usage](docs/USAGE.md)
- [Product Architecture](docs/PRODUCT_ARCHITECTURE.md)
- [API](docs/API.md)
- [Deployment](docs/DEPLOYMENT.md)
- [Roadmap](docs/ROADMAP.md)
- [Visual Prompt Library](docs/visual-prompts/README.md)
- [GitHub Repository Metadata](docs/GITHUB.md)
