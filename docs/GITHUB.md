# GitHub Repository Guide

This file captures the public GitHub presentation for AgentMux. Keep it aligned
with the landing page, README, release notes, and container image workflow.

## Repository Description

Recommended description:

```text
tmux-first remote control plane for long-lived coding agent sessions
```

Recommended homepage:

```text
https://github.com/kinboyw/agentmux
```

Recommended topics:

```text
tmux, terminal, websocket, remote-control, coding-agents, go, cloudflare-tunnel, sqlite
```

Suggested social preview image:

```text
docs/assets/visuals/agentmux-1-system-architecture-v1.png
```

Set with GitHub CLI:

```bash
gh repo edit kinboyw/agentmux \
  --description "tmux-first remote control plane for long-lived coding agent sessions" \
  --homepage "https://github.com/kinboyw/agentmux" \
  --add-topic tmux \
  --add-topic terminal \
  --add-topic websocket \
  --add-topic remote-control \
  --add-topic coding-agents \
  --add-topic go \
  --add-topic cloudflare-tunnel \
  --add-topic sqlite
```

## Product Positioning

Use this short copy in GitHub releases, social cards, and directory listings:

```text
AgentMux is a tmux-first control plane for long-lived coding-agent sessions.
Hub provides the public HTTPS/WSS entrypoint, Worker keeps outbound-only access
to local tmux sessions, and Control gives operators a browser and CLI surface
without requiring agent-specific remote features.
```

## Quick Start Copy

For README and landing-page copy, lead with an existing Hub:

```text
Open an AgentMux Hub landing page, generate a join signal, run the generated
Worker command on the machine that owns your tmux sessions, then open Web
Control from the browser.
```

Keep Hub deployment as a self-hosting path, not the default user path:

```bash
docker run -d --name agentmux --restart unless-stopped -p 8081:8081 ghcr.io/kinboyw/agentmux:latest
```

Keep `go run` examples clearly labeled as source-development commands.

## Cloudflare Copy

For a named Cloudflare Tunnel or any custom domain, `--public-url` is known in
advance:

```bash
agentmux hub --addr 127.0.0.1:8080 --data ./agentmux.db --public-url https://hub.example.com
cloudflared tunnel run agentmux
```

For `cloudflared tunnel --url`, the public `trycloudflare.com` URL is only known
after `cloudflared` starts. Copy the printed URL and restart Hub with that value
before generating Worker or Control commands.

## Release Checklist

- Tag releases as `vX.Y.Z`.
- Confirm CI passes for Go tests and Web Control build.
- Confirm release assets are uploaded for Linux and Darwin.
- Confirm GHCR publishes `ghcr.io/kinboyw/agentmux:X.Y.Z` and `latest`.
- Copy the release note from `docs/releases/vX.Y.Z.md`.
- Verify the landing page top-right version badge links to the GitHub release.
