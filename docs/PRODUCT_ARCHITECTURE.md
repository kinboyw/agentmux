# Product Architecture

## Product Goal

AgentMux should feel like a ZeroTier-style control plane for coding agents:

1. Open the Hub landing page.
2. Generate a short-lived Worker signal and a scoped Direct Token.
3. Paste a one-line command on a worker machine.
4. Open the generated Web Control share URL or use the Direct Token from TUI.
5. Manage long-lived agent sessions without the agent knowing anything about
   remote access.

The core invariant remains:

> Workers own local tmux/PTY state. Hub coordinates identity, discovery, routing,
> and policy. Control renders and operates sessions.

![AgentMux system architecture](assets/visuals/agentmux-1-system-architecture-v1.png)

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

Current deployment flags:

```bash
agentmux hub --addr 127.0.0.1:8080 --data ./agentmux.db --public-url https://hub.example.com
```

- `--data` enables SQLite persistence for identity bootstrap state.
- `--public-url` makes generated commands stable behind Cloudflare Tunnel,
  Cloudflare Proxy, Caddy, Nginx, or another TLS terminator.
- With `cloudflared tunnel --url`, the public `trycloudflare.com` URL is known
  only after `cloudflared` starts. Hub should be restarted with that URL before
  generating Worker and Control commands.
- Without `--data`, Hub keeps the development in-memory auth store.

### Worker

Worker is an outbound connector.

- Joins a hub using a short-lived signal or registered-user token.
- Maintains a long-lived device identity after enrollment.
- Reports local terminal sessions.
- Creates, attaches, resizes, and stops tmux-backed or built-in PTY agent sessions.
- Never requires inbound ports in the default relay mode.

### Control

Control is the user-facing client. It should have multiple implementations:

- Go CLI for debugging and emergency access.
- Web control as the main product surface.
- Future native/mobile control clients if useful.

The browser control should become the primary user experience because it can
render with xterm.js and place UI outside the terminal buffer.

### Web Control Direction

The production web control should be a modern standalone frontend embedded by
the Go Hub for one-command deployment.

Stack:

- React + TypeScript.
- Tailwind CSS.
- shadcn/ui-style primitives.
- xterm.js for terminal rendering.
- resizable pane layout for multi-session work.

Implemented UX:

- Collapsible navigation/sidebar.
- Multi-pane session layout with draggable resize handles.
- Multiple concurrently attached sessions.
- Login/register flow that returns scoped control credentials.
- Direct Token mode for anonymous shared-session access.
- GitHub and Google OAuth entry points behind provider-specific API routes.
- UI status, auth, and session metadata outside terminal buffers.

Deployment model:

- `web/control` is the source frontend.
- `web/control/dist` is copied into `internal/hub/webdist`.
- Hub serves `/control` and `/assets/*` from embedded files.
- Development can still run Vite with API/WS proxy to the local Hub.

Current implementation boundary:

- Registered users can be stored in Hub memory or SQLite.
- Password hashing is a development placeholder using standard-library SHA-256.
- GitHub/Google OAuth routes exist and return a structured "not configured"
  response until provider config and callback handling are implemented.
- Direct Token access is intentionally limited to listing shared sessions,
  opening an existing session stream, and switching sessions. It cannot create
  or stop sessions, generate join signals, list or manage Workers, load
  previews, use workspaces, or trigger version/update features.
- Production auth should add persistent storage, password KDF such as Argon2id
  or external identity only, secure cookies/session rotation, CSRF protection,
  revocation, audit logs, and tenant/workspace policy enforcement.

SQLite is the near-term default because it matches the one-binary deployment
goal. For Cloudflare, the recommended first-class path is:

```text
Browser/Worker/Control -> Cloudflare HTTPS/WSS -> cloudflared tunnel -> Go Hub -> SQLite
```

Cloudflare D1 is a later option for a Workers/Pages-native control plane or an
HTTP storage service. It is not the simplest persistence backend for the current
Go binary because D1 is exposed inside Cloudflare Workers rather than as a local
SQLite file.

![AgentMux Cloudflare deployment](assets/visuals/agentmux-3-cloudflare-deployment-v1.png)

## Onboarding Model

### Anonymous Flow

Anonymous users can generate a temporary Worker join signal and a Control
Direct Token from the Hub landing page.

Properties:

- Short TTL, for example 10 minutes.
- Limited scope.
- Worker signal and Direct Token are tenant-scoped.
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

The landing page presents URLs as links with copy actions, and tokens/commands
as code blocks with copy actions. The generated Web Control share URL carries
the Direct Token:

```text
https://hub.example.com/control?token=amx_cred_xxx
```

The matching TUI path is:

```bash
agentmux-tui --hub https://hub.example.com --token 'amx_cred_xxx'
```

### Registered Flow

Registered users get the full management surface and stronger controls.

Capabilities:

- persistent workers
- multiple controls
- Web Control workspaces
- access policies
- audit logs
- session history
- worker labels
- team sharing
- Worker enable/disable, eviction, and update orchestration

Registered users should still use short-lived join signals for new Workers, but
the generated signal is attached to the registered tenant. Control devices can
use browser sign-in, device login, or generated Direct Tokens depending on the
desired access boundary.

The current prototype exposes:

```text
POST /api/auth/register
POST /api/auth/login
GET  /api/auth/me
POST /api/auth/refresh
POST /api/auth/device/start
POST /api/auth/device/poll
POST /api/auth/device/approve
GET  /api/auth/oauth/{github|google}
```

Register/login issue control credentials for the user's tenant. OAuth routes are
kept as stable frontend integration points before provider configuration lands.

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

Credentials are tenant-scoped and persisted when Hub runs with `--data`.
Production credentials should add revocation and audit trails.

![AgentMux token and tenant model](assets/visuals/agentmux-6-token-and-tenant-model-v1.png)

## Deployment Command

The landing page should generate commands similar to ZeroTier's join flow.

Worker:

```bash
curl -fsSL https://hub.example.com/install.sh | sh -s -- worker \
  --hub wss://hub.example.com \
  --join amx_sig_xxx \
  --name "$(hostname)"
```

Control share:

```bash
agentmux-tui --hub https://hub.example.com --token 'amx_cred_xxx'
```

For local development:

```bash
go run ./cmd/agentmux worker --hub ws://127.0.0.1:8081 --join amx_sig_xxx --name local
```

`--join` performs signal exchange and uses the returned credential as the
runtime bearer token. `--token` remains as a local development/admin override.
Worker keeps a stable local instance id, so the same Worker instance cannot
reuse the same signal by changing only `--name`. If a Worker was accidentally
joined to the wrong Hub, run `agentmux worker leave` before joining again.

The `/install.sh` endpoint is intentionally conservative: it uses an existing
`agentmux` binary from `PATH`, builds from source when run inside a checkout, or
downloads the matching role-specific Linux/macOS release archive from GitHub.
Release assets are split by role so Hub can ship as a smaller cross-platform
binary; Windows support starts with `agentmux-hub-windows-amd64.tar.gz` while
Worker and terminal Control remain Linux/macOS until the Windows PTY/service
model is implemented.

## Update Model

AgentMux should support both installed updates and `npx`-style cached execution.

- Hub and Worker are service-like roles. They should update through staged,
  verified binaries and explicit restarts. Docker Hub deployments should update
  by replacing the container image rather than mutating binaries inside the
  container.
- Control CLI/TUI can support a faster `agentmux run control@latest` workflow
  that downloads a verified binary into a versioned cache and executes it
  without changing the installed binary.
- Remote Worker update should be Hub-orchestrated and backend-aware. It is safe
  by default for tmux-backed sessions because tmux state survives Worker
  restart, but PTY-backed sessions require a disruptive-restart confirmation.
- Compatibility is capability-driven: Hub, Worker, and Control publish protocol
  versions and capabilities, and UI/API paths degrade when an older endpoint is
  missing an optional capability.

See [In-Place Update Strategy](UPDATE_STRATEGY.md) for the command model,
protocol additions, persistence tables, and rollout phases.

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

   A Control credential without a user email is treated as a Direct Token. It is
   scoped to shared-session connection and cannot mutate Hub/Worker state.

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
tenant_id = tenant_<id>
user_id = usr_<id>
```

The immediate implementation stores anonymous and registered tenant identity in
memory or SQLite. Worker connectivity and terminal streams remain runtime state.

## TODO

- Persist tenant records, workers, layouts, revocations, and audit events in
  SQLite.
- Add credential revocation and expiry cleanup.
- Add worker policy: allowed commands, allowed working directories, max sessions.
- Add session ownership/audit events.
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

![AgentMux relay versus direct mode](assets/visuals/agentmux-7-relay-vs-direct-mode-v1.png)

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
7. Vendor web assets and split static files. Done.
8. Persist auth/device state in SQLite. Done for signals, credentials, users.
9. Add headless terminal state sidecar.

## Automation

GitHub Actions now provide two tracks:

- CI on pushes and pull requests: Go tests, Web Control build, binary build.
- Release on `v*` tags: role-specific Linux/macOS tarballs plus Hub-only Windows artifacts.
- Docker image publishing on `v*` tags: multi-arch Linux images are pushed to `ghcr.io/kinboyw/agentmux`.

The release build rebuilds Web Control and embeds it into `internal/hub/webdist`
before compiling the Go binary.
