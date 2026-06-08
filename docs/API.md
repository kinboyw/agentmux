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

Serves the browser control shell. Pass a shared token or short-lived join token
as a query parameter for quick local use:

```text
http://127.0.0.1:8081/control?token=<token>
```

The page uses the same HTTP and WebSocket APIs documented below. Its session
status bar lives outside the terminal buffer, so remote full-screen TUIs and
local shell history do not share terminal state.

### `GET /health`

Returns hub liveness.

```json
{"status":"ok"}
```

### `POST /api/join-tokens`

Mints a short-lived prototype join token. The landing page calls this endpoint
without authentication.

Response:

```json
{
  "token": "amx_join_...",
  "expires_at": "2026-06-08T12:10:00Z",
  "uses_remaining": 2,
  "reusable": true,
  "scopes": ["worker:join", "control:join"],
  "worker_command": "go run ./cmd/agentmux worker --hub ws://127.0.0.1:8081 --join 'amx_join_...' --name $(hostname)",
  "control_command": "go run ./cmd/agentmux control list --hub http://127.0.0.1:8081 --join 'amx_join_...'",
  "control_url": "http://127.0.0.1:8081/control?token=amx_join_..."
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
