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

### `GET /health`

Returns hub liveness.

```json
{"status":"ok"}
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
