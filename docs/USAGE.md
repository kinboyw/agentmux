# AgentMux Usage Guide

This guide covers the normal operating path for AgentMux: run Hub with a stable
public URL, join Workers with a short-lived signal, and control long-running
tmux sessions from the browser.

## Concepts

AgentMux has three roles in one binary:

- `hub`: the HTTPS/WSS entrypoint, API server, Web Control host, and session router.
- `worker`: an outbound connector that manages local tmux sessions on a machine where agents run.
- `control`: CLI commands for listing, creating, and attaching to sessions. The browser control surface is served by Hub at `/control`.

The agent itself remains unaware. Codex, Claude, Gemini, OpenCode, or a shell runs inside tmux; Worker attaches below it at the terminal layer.

## Quick Start With Docker

Run Hub with the published image:

```bash
docker run -d \
  --name agentmux \
  --restart unless-stopped \
  -p 127.0.0.1:8080:8080 \
  -v agentmux-data:/var/lib/agentmux \
  ghcr.io/kinboyw/agentmux:latest \
  hub \
  --addr 0.0.0.0:8080 \
  --data /var/lib/agentmux/agentmux.db \
  --public-url https://hub.example.com
```

Open the public URL, generate a signal, run the Worker command on a machine with
tmux, and then open Web Control.

## Local Smoke Test

For development, run the tmux smoke script:

```bash
./scripts/dev-tmux.sh
```

Open:

```text
http://127.0.0.1:8081/control?token=dev-token
```

This path uses local source builds and is meant for development only.

## Run Hub From A Release Binary

Install the binary from GitHub Releases, then start Hub:

```bash
agentmux hub \
  --addr 0.0.0.0:8080 \
  --data ./agentmux.db \
  --public-url https://hub.example.com
```

Important flags:

- `--addr`: local listen address.
- `--data`: SQLite database path for users, signals, and credentials.
- `--public-url`: external HTTPS URL used to generate worker/control commands.
- `--release-repo`: GitHub `owner/repo` for `/install.sh` downloads. Defaults to `kinboyw/agentmux`.

## Run Hub From Docker Image

Release tags publish a multi-arch image to GitHub Container Registry:

```bash
docker pull ghcr.io/kinboyw/agentmux:latest
```

Run Hub:

```bash
docker run -d \
  --name agentmux \
  --restart unless-stopped \
  -p 127.0.0.1:8080:8080 \
  -v agentmux-data:/var/lib/agentmux \
  ghcr.io/kinboyw/agentmux:latest \
  hub \
  --addr 0.0.0.0:8080 \
  --data /var/lib/agentmux/agentmux.db \
  --public-url https://hub.example.com
```

For local compose-based development:

```bash
cp deploy/agentmux.env.example .env
docker compose up -d --build
```

## Put Hub Behind Cloudflare Tunnel

There are two different Cloudflare Tunnel modes. The `--public-url` value must
match the URL that users, Workers, and Controls will actually use.

### Named tunnel with your own domain

Use this when you already know the public hostname, for example
`https://hub.example.com`. This is the production path.

Start Hub locally:

```bash
agentmux hub \
  --addr 127.0.0.1:8080 \
  --data /var/lib/agentmux/agentmux.db \
  --public-url https://hub.example.com
```

Start the named tunnel:

```bash
cloudflared tunnel run agentmux
```

Cloudflare terminates HTTPS and forwards WebSocket upgrades to Hub. Workers
still connect outbound-only over `wss://...`.

### Quick tunnel with a generated trycloudflare.com URL

For a temporary tunnel, you do not know `--public-url` until `cloudflared`
prints it.

First start Hub with a local URL:

```bash
agentmux hub \
  --addr 127.0.0.1:8080 \
  --data /var/lib/agentmux/agentmux.db \
  --public-url http://127.0.0.1:8080
```

Then run:

```bash
cloudflared tunnel --url http://127.0.0.1:8080
```

Copy the printed `https://*.trycloudflare.com` URL, stop Hub, and restart Hub
with that value:

```bash
agentmux hub \
  --addr 127.0.0.1:8080 \
  --data /var/lib/agentmux/agentmux.db \
  --public-url https://example.trycloudflare.com
```

Then open that public URL and generate Worker/Control commands. Without this
restart, Hub will generate local-only commands.

## Join A Worker

Open the landing page:

```text
https://hub.example.com/
```

Click `Generate join signal`, then run the generated Worker command on the machine that owns the tmux sessions:

```bash
curl -fsSL https://hub.example.com/install.sh | sh -s -- worker --join 'amx_sig_...' --name "$(hostname)"
```

The script uses an existing `agentmux` in `PATH`, builds from source when run inside a checkout, or downloads the matching GitHub release archive.

## Open Web Control

Use the generated Web Control URL:

```text
https://hub.example.com/control?signal=amx_sig_...
```

The browser exchanges the signal for a scoped `amx_cred_...` control credential, then stores it locally.

## CLI Control

Install or bootstrap the control client:

```bash
curl -fsSL https://hub.example.com/install.sh | sh -s -- control --join 'amx_sig_...'
```

Useful source-development commands:

```bash
go run ./cmd/agentmux control list --hub http://127.0.0.1:8081 --token dev-token
go run ./cmd/agentmux control create --hub http://127.0.0.1:8081 --token dev-token --worker local --name demo --cwd "$PWD" --command bash
go run ./cmd/agentmux control attach --hub ws://127.0.0.1:8081 --token dev-token --session local/demo
```

Inside `control attach`, press `Ctrl-]` to detach from the local control stream. The worker-side tmux session keeps running.

## Data And Backups

SQLite persists:

- anonymous join signals
- scoped credentials
- registered users

Runtime state is rebuilt as workers reconnect:

- online workers
- active WebSocket streams
- live tmux session snapshots

Back up the configured `agentmux.db` file for Hub identity state.
