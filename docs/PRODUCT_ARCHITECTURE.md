# Product Architecture

## Product Goal

AgentMux should feel like a ZeroTier-style control plane for coding agents:

1. Open the Hub landing page.
2. Generate a short-lived join signal.
3. Paste a one-line command on a worker machine.
4. Open a control surface from any trusted device.
5. Manage long-lived agent sessions without the agent knowing anything about
   remote access.

The core invariant remains:

> Workers own local tmux/PTY state. Hub coordinates identity, discovery, routing,
> and policy. Control renders and operates sessions.

## Roles

### Hub

Hub is the product entrypoint.

- Landing page.
- Anonymous short-lived join signals.
- Registered-user workspace and policies.
- Worker enrollment.
- Control enrollment.
- WebSocket routing.
- Optional terminal-state service for Plan B.
- Deployment command generator.

Hub should be deployable behind a reverse proxy with HTTPS/WSS. Later it can
serve TLS directly.

### Worker

Worker is an outbound connector.

- Joins a hub using a short-lived signal or registered-user token.
- Maintains a long-lived device identity after enrollment.
- Reports local tmux sessions.
- Creates, attaches, resizes, and stops tmux-backed agent sessions.
- Never requires inbound ports in the default relay mode.

### Control

Control is the user-facing client. It should have multiple implementations:

- Go CLI for debugging and emergency access.
- Web control as the main product surface.
- Future native/mobile control clients if useful.

The browser control should become the primary user experience because it can
render with xterm.js and place UI outside the terminal buffer.

## Onboarding Model

### Anonymous Flow

Anonymous users can generate a temporary join signal from the Hub landing page.

Properties:

- Short TTL, for example 10 minutes.
- Limited scope.
- Limited number of uses.
- No durable account ownership.
- Good for local demos, trusted personal machines, and quick trials.

Suggested signal shape:

```text
amx_join_<random>
```

Hub stores only a hash of the join token plus metadata:

```text
id
token_hash
expires_at
uses_remaining
scopes
created_ip
created_at
```

Initial scopes:

- `worker:join`
- `control:join`

### Registered Flow

Registered users get durable workspaces and stronger controls.

Capabilities:

- persistent workers
- multiple controls
- named workspaces
- access policies
- audit logs
- session history
- worker labels
- team sharing
- direct-mode coordination

Registered users should still use short-lived join signals for new devices, but
the resulting worker/control gets a durable device credential.

## Deployment Command

The landing page should generate commands similar to ZeroTier's join flow.

Worker:

```bash
curl -fsSL https://hub.example.com/install.sh | sh -s -- worker \
  --hub wss://hub.example.com \
  --join amx_join_xxx \
  --name "$(hostname)"
```

Control CLI:

```bash
curl -fsSL https://hub.example.com/install.sh | sh -s -- control \
  --hub https://hub.example.com \
  --join amx_join_xxx
```

For local development:

```bash
go run ./cmd/agentmux worker --hub ws://127.0.0.1:8081 --join amx_join_xxx --name local
```

The first prototype can use the join token directly as the runtime bearer token.
Production should exchange the join token for a durable device credential.

## Trust and Token Model

### Prototype

- `--token` remains for local development.
- Hub landing page can mint ephemeral tokens.
- Worker/control can pass `--token` or later `--join`.

### Product

Use two token classes:

1. Join signal
   - short-lived
   - one-time or limited-use
   - exchanged for device credentials

2. Device credential
   - long-lived
   - scoped to worker/control
   - revocable from Hub

## Routing Modes

### Relay Mode

Default mode.

```text
worker -> hub <- control
```

Pros:

- works behind NAT
- simplest user experience
- one public endpoint

Cons:

- hub bandwidth and latency bottleneck
- terminal bytes pass through hub

### Direct Mode

Future optimization.

```text
worker <-> control
hub = signaling + auth + rendezvous
```

Candidates:

- same-LAN direct WebSocket
- WireGuard/Tailscale-like private addresses
- WebRTC data channel
- QUIC hole punching if needed later

Hub responsibilities in direct mode:

- authenticate both sides
- exchange endpoint candidates
- authorize session access
- fall back to relay mode

Direct mode should be optional. Relay mode remains the reliable default.

## Plan B: Headless Terminal State

Plan B is the path to stable reconnects and perfect UI overlays.

Recommended stack:

- Browser control: TypeScript + xterm.js.
- Headless terminal state: `@xterm/headless` + `@xterm/addon-serialize`.
- Hub or sidecar consumes terminal bytes and maintains screen state.

Benefits:

- reconnect snapshot
- no scrollback pollution
- DOM overlay status bars
- tabs and split panes
- multi-control fanout
- eventual replay/session history

The Go CLI should remain a debug tool. The product-grade control should be web.

## Initial Implementation Plan

1. Add Hub landing page.
2. Add anonymous join-token generation endpoint.
3. Show one-line worker and control commands.
4. Add `--join` alias to worker/control CLI.
5. Add basic in-memory join-token store.
6. Later persist token/device state in SQLite.
7. Add browser control shell with xterm.js.
8. Add headless terminal state sidecar.

