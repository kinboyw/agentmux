# AgentMux API

AgentMux uses HTTP for request/response operations and WebSocket for worker and
control streams.

All authenticated HTTP requests use:

```text
Authorization: Bearer <token>
```

WebSocket endpoints accept either the same Authorization header or:

```text
?token=<token>
```

## HTTP API

### `GET /control`

Serves the browser control shell. Pass either a development/admin token or a
short-lived signal/direct token:

```text
http://127.0.0.1:8081/control?token=<token>
http://127.0.0.1:8081/control?signal=<amx_sig_...>
```

The page uses the same HTTP and WebSocket APIs documented below. Its session
status bar lives outside the terminal buffer, so remote full-screen TUIs and
local shell history do not share terminal state.

### `GET /`

Serves the Hub landing page. The page is intentionally operational: it can mint
signals, display worker/control commands, and link directly into Web Control.

### `GET /install.sh`

Serves a small bootstrap script used by the landing page.

Worker mode:

```bash
curl -fsSL https://hub.example.com/install.sh | sh -s -- worker --join 'amx_sig_...' --name "$(hostname)"
```

Control mode:

```bash
curl -fsSL https://hub.example.com/install.sh | sh -s -- control
```

The script uses an existing role binary in `PATH`, builds from source when run
inside a checkout, or downloads a matching role-specific Linux/macOS release
archive from GitHub. Worker install commands prefer `agentmux-worker-*`.
Control installs a real `agentmux-tui` binary and prefers `agentmux-tui-*`,
with fallback to older `agentmux-control-*` and legacy `agentmux-*` assets.
Hub-only Windows artifacts are published separately as
`agentmux-hub-windows-amd64.tar.gz`.

### `GET /deploy-hub.sh`

Serves a Linux/systemd self-host deployment script for Hub:

```bash
curl -fsSL https://hub.example.com/deploy-hub.sh | sh
```

Common overrides:

```bash
AGENTMUX_PUBLIC_URL=https://hub.example.com \
AGENTMUX_ADDR=127.0.0.1:8081 \
AGENTMUX_DATA=/var/lib/agentmux/agentmux.db \
curl -fsSL https://hub.example.com/deploy-hub.sh | sh
```

The script downloads and verifies the `agentmux-hub-linux-${arch}` release
archive, installs the binary, writes `/etc/agentmux/agentmux.env`, creates a
systemd unit, creates the configured system user/group, fixes data/env
ownership, starts `agentmux.service`, and checks `/health`.

### `GET /run.sh`

Serves an `npx`-style cached runner. It downloads and verifies a release asset
under `~/.cache/agentmux`, then executes it without changing the installed
binary.

```bash
curl -fsSL https://hub.example.com/run.sh | sh -s -- control@latest
curl -fsSL https://hub.example.com/run.sh | sh -s -- hub@v0.1.0 --addr 127.0.0.1:8081
```

Control executes the cached `agentmux-tui` binary with `--hub <current-hub>`.
Worker defaults to `worker --hub <current-hub-ws>`.

### `GET /health`

Returns hub liveness.

```json
{"status":"ok"}
```

### `GET /api/version`

Returns Hub build metadata. This endpoint is unauthenticated so the landing page
and Web Control can detect refresh/update opportunities.

```json
{
  "role": "hub",
  "version": "v0.1.0",
  "commit": "abc1234",
  "build_time": "2026-06-10T12:00:00Z",
  "go_version": "go1.26.4",
  "os": "linux",
  "arch": "amd64",
  "protocol_version": "1",
  "capabilities": ["api.version", "worker.software_inventory"],
  "compatibility": {
    "worker_protocol": {"min": "1", "preferred": "1"},
    "control_protocol": {"min": "1", "preferred": "1"}
  },
  "release_repo": "kinboyw/agentmux"
}
```

### `POST /api/signals`

Mints a short-lived Worker bootstrap signal and a scoped Control Direct Token.
This endpoint supports anonymous generation when no token is supplied. If a
registered Control credential or admin token is supplied, the generated material
is scoped to that tenant. Direct Token credentials cannot call this endpoint,
and invalid supplied credentials return `401` instead of silently falling back
to anonymous generation.

A raw signal cannot call normal API or WebSocket routes directly; Worker must
exchange it for a credential. The generated Direct Token is already a scoped
Control credential and is limited to simple shared-session access.

Response:

```json
{
  "token": "amx_sig_...",
  "signal": "amx_sig_...",
  "signal_id": "sig_...",
  "tenant_id": "anon_...",
  "expires_at": "2026-06-09T12:10:00Z",
  "uses_remaining": -1,
  "reusable": true,
  "scopes": ["worker:join"],
  "worker_command": "curl -fsSL http://127.0.0.1:8081/install.sh | sh -s -- worker --join 'amx_sig_...' --name \"$(hostname)\"",
  "worker_join_command": "agentmux worker join --hub 'ws://127.0.0.1:8081' --join 'amx_sig_...' --name \"$(hostname)\"",
  "direct_token": "amx_cred_...",
  "direct_token_id": "cred_...",
  "direct_token_expires_at": "2026-06-09T12:10:00Z",
  "control_share_url": "http://127.0.0.1:8081/control?token=amx_cred_...",
  "control_direct_command": "agentmux-tui --hub 'http://127.0.0.1:8081' --token 'amx_cred_...'",
  "control_command": "agentmux-tui --hub 'http://127.0.0.1:8081'",
  "control_url": "http://127.0.0.1:8081/control"
}
```

`POST /api/join-tokens` remains as a compatibility alias for now.

If the Hub is started with `--public-url https://hub.example.com`, generated
commands and `control_url` use that URL instead of request-local host headers.
Use a stable hostname for production. With `cloudflared tunnel --url`, copy the
printed `https://*.trycloudflare.com` URL and restart Hub with that value before
generating join commands.

Direct Token access is intentionally restricted. It can list shared sessions and
open existing session streams. It cannot generate new signals, list or manage
Workers, create/stop sessions, send REST input, load previews, inspect targets,
open targeted panes, or use registered-account features.

### `POST /api/exchange`

Exchanges a signal for a scoped runtime credential.

Request:

```json
{
  "signal": "amx_sig_...",
  "role": "worker",
  "device_id": "optional-stable-device-id",
  "device_name": "laptop"
}
```

Response:

```json
{
  "credential": "amx_cred_...",
  "credential_id": "cred_...",
  "tenant_id": "anon_...",
  "role": "worker",
  "device_id": "dev_...",
  "expires_at": "2026-06-10T12:00:00Z",
  "scopes": ["worker:connect", "session:report", "terminal:stream"]
}
```

### `GET /api/workers`

Returns registered workers visible to the credential tenant. Online workers are
backed by a live WebSocket connection; offline workers remain visible from Hub
persistence after disconnects and Hub restarts. Direct Token credentials may
read worker status and backend metadata, but cannot manage workers.

```json
{
  "workers": [
    {
      "id":"local",
      "worker_instance_id":"wins_...",
      "name":"local",
      "addr":"127.0.0.1",
      "backend":"tmux",
      "software":{
        "version":"v0.1.0",
        "commit":"abc1234",
        "protocol_version":"1",
        "capabilities":["session.snapshot","terminal.open"],
        "os":"linux",
        "arch":"amd64",
        "service_backend":"systemd-user",
        "update_policy":"manual"
      },
      "last_seen":"2026-06-08T12:00:00Z",
      "status":"online",
      "online":true
    }
  ]
}
```

Identity model:

- `id` / `worker_id` is the routable Worker ID used in session IDs such as
  `local/demo`. It is user-facing and may later support rename/migration flows.
- `worker_instance_id` is generated once on the Worker installation and is
  stable across Worker renames. Hub stores it for auditing, update tracking, and
  future rename-safe identity. Older Workers may omit it.

### `PATCH /api/workers/{worker}`

Updates runtime controls for a registered worker. This is a tenant-scoped
operation; direct shared control tokens cannot manage workers.

Request:

```json
{"enabled":true,"trace_enabled":false,"debug_enabled":false}
```

Response:

```json
{"status":"updated"}
```

### `DELETE /api/workers/{worker}`

Evicts a Worker from the current tenant. Hub closes the live Worker connection
when it is online, removes the runtime Worker/session snapshots, and returns the
last visible Worker record. This lets Control clean up an accidental or stale
Worker join before re-joining the correct instance. Direct Token credentials
receive `403`.

Response:

```json
{"status":"evicted","worker":{"id":"local","name":"local","online":false}}
```

### `GET /api/workers/{worker}/updates`

Returns durable update jobs for a worker. SQLite-backed Hubs persist jobs and
events across restarts; in-memory development Hubs keep the same API surface but
do not survive process restarts.

Response:

```json
{
  "jobs": [
    {
      "id": "upd_...",
      "worker_id": "mywsl",
      "worker_instance_id": "wins_...",
      "target_version": "latest",
      "repo": "kinboyw/agentmux",
      "status": "sent",
      "message": "waiting for worker",
      "created_at": "2026-06-10T12:00:00Z",
      "updated_at": "2026-06-10T12:00:01Z",
      "events": [
        {
          "id": "evt_...",
          "job_id": "upd_...",
          "worker_id": "mywsl",
          "worker_instance_id": "wins_...",
          "status": "queued",
          "message": "update queued",
          "created_at": "2026-06-10T12:00:00Z"
        }
      ]
    }
  ]
}
```

### `POST /api/workers/{worker}/updates`

Queues a remote Worker binary update. Hub sends `worker.update.apply` to the
connected Worker, the Worker stages a verified release with `agentmux update
apply`, then restarts itself. For tmux-backed Workers, sessions keep running
inside tmux. For built-in PTY Workers, the request must explicitly allow a
disruptive restart.

Request:

```json
{"version":"latest","allow_disruptive_restart":false}
```

Response:

```json
{
  "job": {
    "id": "upd_...",
    "worker_id": "mywsl",
    "target_version": "latest",
    "status": "sent",
    "created_at": "2026-06-10T12:00:00Z",
    "updated_at": "2026-06-10T12:00:00Z"
  }
}
```

Possible errors:

- `404`: worker is unknown.
- `403`: worker belongs to another tenant, the worker is disabled, or the token
  is not allowed to create sessions in that tenant.
- `409`: worker is offline, disabled, missing `worker.update.apply`, or uses
  `pty` without disruptive restart confirmation.

### `GET /api/sessions`

Returns sessions currently reported by workers.

```json
{
  "sessions": [
    {
      "id":"local/demo",
      "worker_id":"local",
      "name":"demo",
      "cwd":"/repo",
      "command":"codex",
      "status":"active"
    }
  ]
}
```

### `POST /api/sessions`

Creates a tmux session on a worker.

Request:

```json
{"worker_id":"local","name":"demo","cwd":"/repo","command":"codex"}
```

Response:

```json
{"status":"created"}
```

The hub waits for the worker to confirm the session creation. Worker-side CWD
or command validation errors are returned as `400 Bad Request`.

### `GET /api/sessions/{worker}/{name}/preview`

Returns a terminal preview for a session.

Query parameters:

- `lines`: maximum preview lines, default `80`

Response:

```json
{"data":"...","scope":"active_pane"}
```

### `GET /api/sessions/{worker}/{name}/targets`

Returns attachable terminal targets for a session. tmux workers report one item
per pane across all windows so narrow clients can navigate to the pane level.
Workers without pane-aware backends may return a single session-level target.

Response:

```json
{
  "targets": [
    {
      "session_name": "demo",
      "window_id": "@1",
      "window_index": 0,
      "window_name": "main",
      "pane_id": "%1",
      "pane_index": 0,
      "pane_active": true,
      "cwd": "/repo",
      "command": "codex",
      "width": 80,
      "height": 24
    }
  ]
}
```

### `POST /api/sessions/{worker}/{name}/input`

Sends terminal input to a session.

Request:

```json
{"data":"hello\n"}
```

Response:

```json
{"status":"queued"}
```

### `DELETE /api/sessions/{worker}/{name}`

Stops a tmux session.

Response:

```json
{"status":"queued"}
```

## WebSocket Envelope

All WebSocket messages are JSON envelopes:

```json
{
  "type": "terminal.output",
  "id": "optional-request-id",
  "stream_id": "optional-terminal-stream-id",
  "worker_id": "local",
  "session_id": "local/demo",
  "payload": {}
}
```

`payload` is type-specific.

## Worker WebSocket

Endpoint:

```text
GET /ws/worker?token=<token>
```

### `worker.hello`

Sent by worker after connect.

```json
{"type":"worker.hello","worker_id":"local","payload":{"name":"local","version":"dev"}}
```

### `worker.heartbeat`

Sent periodically.

```json
{"type":"worker.heartbeat","worker_id":"local"}
```

### `session.snapshot`

Sent by worker after connect and whenever sessions change.

```json
{
  "type":"session.snapshot",
  "worker_id":"local",
  "payload":{"sessions":[{"name":"demo","cwd":"/repo","command":"bash","status":"active"}]}
}
```

### Hub to Worker Messages

- `session.create`
- `session.kill`
- `terminal.open`
- `terminal.close`
- `terminal.input`
- `terminal.resize`

## Control WebSocket

Endpoint:

```text
GET /ws/control?token=<token>
```

### `control.open`

Subscribe to a session output stream.

```json
{"type":"control.open","session_id":"local/demo"}
```

The current CLI client includes a generated `stream_id` so the Hub can route
output to the exact attach connection.
The payload includes the current terminal size:

```json
{"cols":120,"rows":36}
```

## Persistence Boundary

SQLite persistence is enabled by:

```bash
agentmux hub --data ./agentmux.db
```

Persisted:

- anonymous signals
- exchanged credentials
- registered users

Runtime-only:

- currently connected workers
- active WebSocket streams
- live session snapshots

Workers reconnect and resubmit snapshots after a Hub restart. This keeps SQLite
as the durable identity/policy store while tmux remains the source of terminal
truth on each worker.

### `control.input`

Send input to a session.

```json
{"type":"control.input","session_id":"local/demo","payload":{"data":"pwd\n"}}
```

### `terminal.resize`

Resize the worker-side PTY for an attach stream.

```json
{"type":"terminal.resize","stream_id":"...","session_id":"local/demo","payload":{"cols":160,"rows":48}}
```

### `terminal.output`

Hub forwards worker output to subscribed controls.

```json
{"type":"terminal.output","session_id":"local/demo","payload":{"data":"..."}}
```
