# Deployment

AgentMux Hub is a single Go binary. The production path is to run the Hub on a
server, keep SQLite on local disk, and put HTTPS/WSS in front of it with
Cloudflare Tunnel or another reverse proxy.

![AgentMux Cloudflare deployment](assets/visuals/agentmux-3-cloudflare-deployment-v1.png)

## Binary + Cloudflare Tunnel

Install a release binary into `/usr/local/bin/agentmux`, then create config:

```bash
sudo useradd --system --home /var/lib/agentmux --shell /usr/sbin/nologin agentmux
sudo install -d -o agentmux -g agentmux /var/lib/agentmux
sudo install -d /etc/agentmux
sudo install -m 0640 deploy/systemd/agentmux.env.example /etc/agentmux/agentmux.env
sudo install -m 0644 deploy/systemd/agentmux.service /etc/systemd/system/agentmux.service
sudo systemctl daemon-reload
sudo systemctl enable --now agentmux
```

Edit `/etc/agentmux/agentmux.env`:

```text
AGENTMUX_TOKEN=change-me-long-random-admin-token
AGENTMUX_PUBLIC_URL=https://hub.example.com
AGENTMUX_DATA=/var/lib/agentmux/agentmux.db
```

Cloudflare quick tunnel:

```bash
cloudflared tunnel --url http://127.0.0.1:8080
```

Named tunnel:

```bash
cloudflared tunnel create agentmux
cloudflared tunnel route dns agentmux hub.example.com
sudo install -m 0644 deploy/cloudflare/config.yml.example /etc/cloudflared/config.yml
cloudflared tunnel run agentmux
```

The Hub must be started with:

```bash
agentmux hub \
  --addr 127.0.0.1:8080 \
  --data /var/lib/agentmux/agentmux.db \
  --public-url https://hub.example.com
```

`--public-url` is what makes generated worker commands use
`wss://hub.example.com`.

## Docker Compose

Release tags publish a multi-arch Hub image to GitHub Container Registry:

```bash
docker pull ghcr.io/kinboyw/agentmux:latest
```

Run a released image directly:

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

Local compose remains source-build oriented:

Create `.env` from the example:

```bash
cp deploy/agentmux.env.example .env
```

Run:

```bash
docker compose up -d --build
```

The compose file binds Hub to `127.0.0.1:8080` by default, so it is ready for
Cloudflare Tunnel:

```bash
cloudflared tunnel --url http://127.0.0.1:8080
```

## Generated Install Script

The Hub serves:

```text
GET /install.sh
```

The landing page uses it for one-line worker/control commands:

```bash
curl -fsSL https://hub.example.com/install.sh | sh -s -- worker --join 'amx_sig_...' --name "$(hostname)"
curl -fsSL https://hub.example.com/install.sh | sh -s -- control --join 'amx_sig_...'
```

By default the script downloads release assets from:

```text
https://github.com/kinboyw/agentmux/releases
```

Override this at Hub startup if your repo path differs:

```bash
agentmux hub --release-repo your-org/agentmux ...
```

Or override from the client side:

```bash
AGENTMUX_REPO=your-org/agentmux curl -fsSL https://hub.example.com/install.sh | sh -s -- worker --join 'amx_sig_...'
```

## Data Boundary

SQLite persists:

- anonymous signals
- scoped credentials
- registered users

Runtime state is rebuilt as workers reconnect:

- connected workers
- active WebSocket streams
- live session snapshots

Back up `/var/lib/agentmux/agentmux.db` for Hub identity state.
