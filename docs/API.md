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
short-lived signal:

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
curl -fsSL https://hub.example.com/install.sh | sh -s -- control --join 'amx_sig_...'
```

Until release binaries are published, the script expects either `agentmux` to
already be in `PATH` or to be run from a source checkout with Go available.

### `GET /health`

Returns hub liveness.

```json
{"status":"ok"}
```

### `POST /api/signals`

Mints a short-lived bootstrap signal. The landing page calls this endpoint
without authentication. A signal cannot call normal API or WebSocket routes
directly; worker and control clients must exchange it for a credential.

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
  "scopes": ["worker:join", "control:join"],
  "worker_command": "curl -fsSL http://127.0.0.1:8081/install.sh | sh -s -- worker --join 'amx_sig_...' --name \"$(hostname)\"",
  "control_command": "curl -fsSL http://127.0.0.1:8081/install.sh | sh -s -- control --join 'amx_sig_...'",
  "control_url": "http://127.0.0.1:8081/control?signal=amx_sig_..."
}
```

`POST /api/join-tokens` remains as a compatibility alias for now.

If the Hub is started with `--public-url https://hub.example.com`, generated
commands and `control_url` use that URL instead of request-local host headers.
This is the recommended mode behind Cloudflare Tunnel or another reverse proxy.

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

Returns connected workers.

```json
{
  "workers": [
    {"id":"local","name":"local","addr":"127.0.0.1","last_seen":"2026-06-08T12:00:00Z"}
  ]
}
```

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
{"status":"queued"}
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
