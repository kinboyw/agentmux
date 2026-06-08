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

Signal shape:

```text
amx_sig_<random>
```

Hub stores only a hash of the signal token plus metadata:

```text
id
token_hash
tenant_id
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

### Signal Exchange

Signals are bootstrap material only. Worker and control clients must exchange a
signal for a scoped credential before using normal HTTP or WebSocket APIs.

```text
POST /api/exchange
{
  "signal": "amx_sig_xxx",
  "role": "worker|control",
  "device_name": "laptop",
  "device_id": "optional-stable-device-id"
}
```

Response:

```json
{
  "credential": "amx_cred_...",
  "credential_id": "cred_...",
  "role": "worker",
  "tenant_id": "anon_...",
  "expires_at": "2026-06-09T12:00:00Z"
}
```

The current in-memory implementation may issue reusable credentials for a short
period. Production credentials should be revocable, auditable, tenant-scoped,
and persisted.

## Deployment Command

The landing page should generate commands similar to ZeroTier's join flow.

Worker:

```bash
curl -fsSL https://hub.example.com/install.sh | sh -s -- worker \
  --hub wss://hub.example.com \
  --join amx_sig_xxx \
  --name "$(hostname)"
```

Control CLI:

```bash
curl -fsSL https://hub.example.com/install.sh | sh -s -- control \
  --hub https://hub.example.com \
  --join amx_sig_xxx
```

For local development:

```bash
go run ./cmd/agentmux worker --hub ws://127.0.0.1:8081 --join amx_sig_xxx --name local
```

`--join` performs signal exchange and uses the returned credential as the
runtime bearer token. `--token` remains as a local development/admin override.

## Trust and Token Model

Use three token classes:

1. Join signal
   - short-lived
   - one-time or limited-use
   - exchanged for device credentials

2. Device credential
   - long-lived
   - scoped to worker/control
   - revocable from Hub

3. Hub admin token
   - deployment/local-development control plane override
   - never shown in anonymous landing URLs
   - not a normal worker/control runtime credential

## Multi-Tenant Model

Every signal, credential, worker, control, and session should carry a
`tenant_id`.

Anonymous onboarding creates a temporary tenant:

```text
tenant_id = anon_<random>
expires_at = signal expiry or short anonymous workspace TTL
```

Registered onboarding attaches signals and credentials to a durable tenant:

```text
tenant_id = org_<id>
user_id = usr_<id>
```

The immediate implementation can keep this in memory, but the API and data
model should avoid assuming a single global namespace.

## TODO

- Persist signals, credentials, tenants, workers, and audit events in SQLite.
- Add explicit scopes to every API and WebSocket route.
- Add credential revocation and expiry cleanup.
- Vendor xterm.js assets into Go embed; remove CDN dependency.
- Split Web control code into static files before it grows further.
- Add worker policy: allowed commands, allowed working directories, max sessions.
- Add session ownership/audit events.
- Add registered-user auth and tenant selection.
- Add direct-mode signaling after relay mode is stable.

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

1. Add Hub landing page. Done.
2. Add anonymous signal generation endpoint. Done.
3. Show one-line worker and control commands. Done.
4. Add `--join` exchange flow to worker/control CLI. Done.
5. Add basic in-memory signal and credential stores. Done.
6. Add browser control shell with xterm.js. Done.
7. Vendor web assets and split static files.
8. Persist auth/device state in SQLite.
9. Add headless terminal state sidecar.
