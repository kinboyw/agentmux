# In-Place Update Strategy

AgentMux has four runtime surfaces that need different update behavior:

- Hub: long-running public control plane.
- Worker: long-running outbound connector that owns local tmux/PTY sessions.
- Web Control: browser assets served by Hub.
- Control CLI/TUI: local operator client.

The product goal is an `npx`-like experience for first use and day-two
operation, while keeping service updates explicit, verifiable, and recoverable.

## Goals

- Install or run a role with one command.
- Re-run the same command safely when a binary is already installed.
- Support pinned versions and `latest`.
- Verify downloaded artifacts before execution.
- Update Worker without destroying tmux-backed sessions.
- Show version and update state in Web Control.
- Keep Docker Hub updates aligned with normal container workflows.

## Non-Goals

- Silent auto-update of Hub or Worker by default.
- Replacing package managers, Homebrew, apt, Docker, or systemd.
- Updating live attached Control TUI sessions mid-attach.
- Solving Windows Worker/Control before the PTY/service model is native.

## Current Foundation

The project already has the pieces needed for a staged implementation:

- `/install.sh` is Hub-served, role-aware, and downloads release assets.
- Release assets are split by role: Worker, Control, and Hub.
- Worker startup can install or restart a user service through
  `internal/workerservice`.
- Worker uses tmux as the primary session backend, so Worker restart does not
  need to terminate existing tmux sessions.
- Hub can expose the configured release repository through `--release-repo`.

## Update Modes

### 1. Bootstrap Install

Used when a machine has no AgentMux binary yet.

```bash
curl -fsSL https://hub.example.com/install.sh | sh -s -- worker --join 'amx_sig_...' --name "$(hostname)"
curl -fsSL https://hub.example.com/install.sh | sh -s -- control
curl -fsSL https://hub.example.com/install.sh | sh -s -- hub --addr 0.0.0.0:8081
```

The script should stay small: resolve platform, download the role asset, verify
it, install it into `~/.local/bin` by default, then delegate to the binary.

### 2. Local Self-Update

Used by an already installed binary.

```bash
agentmux update check
agentmux update apply --role control --version latest
agentmux update apply --role worker --version v0.1.0
agentmux update rollback
```

Implementation should live behind an `internal/updater` package:

```text
Check -> ResolveRelease -> Download -> Verify -> Stage -> Swap -> PostAction
```

The updater must never overwrite the running executable directly. It stages the
new binary next to the current install, verifies it, atomically swaps a symlink
or file path, and keeps the previous binary for rollback.

### 3. npx-Style Cached Run

Used when the user wants to run a role without mutating the installed binary.

```bash
agentmux run control@latest --hub https://hub.example.com
agentmux run control@v0.1.0 --hub https://hub.example.com
```

When no AgentMux binary exists, Hub can expose a thin shell bootstrap:

```bash
curl -fsSL https://hub.example.com/run.sh | sh -s -- control@latest
```

The runner downloads a verified asset into a versioned cache and executes it:

```text
~/.cache/agentmux/
  releases/
    v0.1.0/
      control/linux-amd64/agentmux
      worker/linux-amd64/agentmux
      hub/linux-amd64/agentmux-hub
  metadata/
    kinboyw_agentmux_latest.json
```

This mode is best for Control CLI/TUI. It is acceptable for one-off Hub demos,
but production Hub and Worker should use installed mode so service managers and
logs remain predictable.

### 4. Remote Worker Update

Used by Web Control or Control TUI to update a registered Worker.

```text
Control -> Hub: create update job for worker mywsl
Hub -> Worker: worker.update.apply(version, artifact, checksum)
Worker: download, verify, stage
Worker: report ready
Worker: restart service
Worker -> Hub: reconnect with new version
Hub: mark job succeeded
```

Policy depends on session backend:

- `tmux`: update can restart Worker because tmux sessions continue running.
- `pty`: update should require confirmation because in-process PTY sessions may
  be lost.
- `auto`: Hub should use the concrete backend reported by Worker.

Remote update should be opt-in per job. Fleet auto-update can be added later
with a maintenance window and a maximum concurrency limit.

### 5. Hub Update

Hub update depends on deployment type.

Binary/systemd:

```bash
agentmux-hub update apply --version latest --restart
```

Docker:

```bash
docker pull ghcr.io/kinboyw/agentmux:latest
docker stop agentmux
docker rm agentmux
docker run -d --name agentmux --restart unless-stopped -p 8081:8081 ghcr.io/kinboyw/agentmux:latest
```

Docker deployments should not self-rewrite binaries inside the container. Web
Control can show that a newer Hub image exists, but the update command should be
copyable documentation rather than an automatic in-container mutation.

### 6. Web Control Update

Web Control is updated when Hub is updated because the assets are embedded into
Hub.

The browser should poll a lightweight version endpoint:

```http
GET /api/version
```

Example:

```json
{
  "version": "v0.1.0",
  "commit": "abc1234",
  "build_time": "2026-06-10T12:00:00Z",
  "web_asset_hash": "sha256:...",
  "release_repo": "kinboyw/agentmux"
}
```

When `web_asset_hash` changes, Web Control shows a non-blocking toast:

```text
New AgentMux Web Control is available. Refresh when convenient.
```

## Release Metadata

The release workflow should publish a machine-readable manifest in addition to
tarballs and `.sha256` files:

```json
{
  "version": "v0.1.0",
  "channel": "stable",
  "repo": "kinboyw/agentmux",
  "created_at": "2026-06-10T12:00:00Z",
  "assets": [
    {
      "role": "worker",
      "os": "linux",
      "arch": "amd64",
      "name": "agentmux-worker-linux-amd64.tar.gz",
      "sha256": "...",
      "size": 12345678
    }
  ]
}
```

Near term, `.sha256` verification is enough to prevent corrupt downloads. Before
remote Worker update is enabled by default, release manifests should also be
signed with `cosign`, `minisign`, or another small signature verifier.

## Compatibility Model

AgentMux should not rely on semantic version comparison alone. Hub, Worker, Web
Control, and Control TUI may be released independently, and a newer binary can
still be incompatible if a required protocol capability is missing.

Compatibility is decided in this order:

1. Protocol major version.
2. Required capabilities for the requested operation.
3. Product version policy for warnings, upgrade prompts, and support windows.

Current protocol baseline:

```text
protocol_version = 1
```

Compatibility matrix:

| Hub protocol | Worker protocol | Control protocol | Result |
| --- | --- | --- | --- |
| `1` | `1` | `1` | Fully supported for current relay mode. |
| `1` | empty/legacy | `1` | Allow connection, hide capability-specific actions, recommend Worker update. |
| `1` | `1` | empty/legacy | Allow basic HTTP/WS operations, recommend Control update. |
| `1` | `2+` | `1` | Allow only if Worker advertises compatible v1 capabilities; otherwise reject or require Hub update. |
| `2+` | `1` | `1` | Hub must keep a v1 compatibility adapter or explicitly reject with an upgrade message. |

Required operation capabilities:

| Operation | Required Worker capability | Fallback |
| --- | --- | --- |
| List sessions | `session.snapshot` | No fallback. |
| Create session | `session.create` | Disable create button and return `403`. |
| Stop session | `session.kill` | Disable stop/kill UI. |
| Preview card | `session.preview.active_pane` | Show metadata-only card. |
| Pane target picker | `session.targets` | Attach to default session target. |
| Attach whole session | `terminal.open` | No fallback. |
| Attach specific pane | `terminal.target_attach` | Attach whole session instead. |
| Remote resize | `terminal.resize` | Keep initial size and show degraded state. |
| Worker version badges | `worker.software_inventory` | Show `legacy` version state. |
| Remote Worker update | `worker.update.apply` | Disable update action and show manual update command. |

Compatibility policy:

- Hub accepts same-major protocol clients by default.
- Hub may accept legacy clients only when the requested action has a safe
  fallback.
- Hub should return structured errors for incompatible actions:

```json
{
  "error": "worker capability missing",
  "capability": "session.targets",
  "fallback": "attach_default_target"
}
```

- Remote update must never install a version whose protocol major is outside
  the Hub-supported range unless the job explicitly targets a Hub upgrade path.
- Worker update success is confirmed only after reconnect with the expected
  product version and compatible protocol metadata.
- Control clients should warn when older than the Hub preferred version, but
  should continue if required capabilities are available.

The first implementation exposes compatibility metadata through:

```http
GET /api/version
GET /api/workers
```

Worker hello includes:

```json
{
  "name": "mywsl",
  "backend": "tmux",
  "version": "v0.1.0",
  "protocol_version": "1",
  "capabilities": ["session.snapshot", "terminal.open", "worker.software_inventory"]
}
```

## Protocol Additions

`worker.hello` already carries `version`. It should grow into a software
inventory payload:

```json
{
  "name": "mywsl",
  "version": "v0.1.0",
  "commit": "abc1234",
  "backend": "tmux",
  "os": "linux",
  "arch": "amd64",
  "protocol_version": "1",
  "capabilities": ["session.snapshot", "session.targets", "terminal.open"],
  "install_kind": "service",
  "service_backend": "systemd-user",
  "update_channel": "stable",
  "update_policy": "manual"
}
```

Message types:

```text
worker.update.apply
worker.update.result
```

`worker.update.apply` currently includes:

```json
{
  "job_id": "upd_...",
  "version": "latest",
  "role": "worker",
  "repo": "kinboyw/agentmux",
  "restart": true,
  "allow_disruptive_restart": false
}
```

The Worker must reject an update if:

- role, OS, or architecture does not match;
- checksum or signature fails;
- the requested version is older and rollback was not explicitly requested;
- the target protocol major is outside the Hub-supported compatibility matrix;
- the target release does not advertise capabilities required by this tenant or worker policy;
- backend is `pty` and the job does not allow disruptive restart;
- another update job is already running.

## Hub API Additions

```http
GET  /api/version
GET  /api/releases/latest
GET  /api/workers/{worker_id}/updates
POST /api/workers/{worker_id}/updates
POST /api/workers/{worker_id}/updates/{job_id}/cancel
```

Create update job:

```json
{
  "version": "latest",
  "channel": "stable",
  "restart": true,
  "allow_disruptive_restart": false
}
```

Job response:

```json
{
  "id": "upd_...",
  "worker_id": "mywsl",
  "target_version": "v0.1.0",
  "status": "staged|running|succeeded|failed|cancelled",
  "message": "waiting for worker reconnect",
  "created_at": "2026-06-10T12:00:00Z",
  "updated_at": "2026-06-10T12:00:03Z"
}
```

## Persistence

SQLite should persist software inventory and update jobs:

```text
devices
  software_version
  software_commit
  protocol_version
  capabilities_json
  os
  arch
  install_kind
  service_backend
  update_channel
  update_policy
  last_update_at

update_jobs
  id
  tenant_id
  device_id
  requested_by
  target_version
  status
  artifact
  checksum
  message
  created_at
  updated_at
  finished_at

update_events
  id
  job_id
  level
  message
  created_at
```

Runtime state can still be rebuilt from Worker reconnects. Update history should
remain durable because it is operational audit data.

The first remote Worker update implementation keeps update jobs in Hub memory.
That is enough for a manual Web Control action and reconnect-based success
confirmation, but it is not an audit trail. Persist `update_jobs` and
`update_events` before enabling fleet-wide or unattended update policies.

## UX

Web Control should show:

- Hub version in the top bar.
- Worker version badge in Worker cards and session lists.
- Worker protocol/capability state in Worker cards.
- `Update available` badge when Hub resolves a newer compatible release.
- Copyable local update commands for each role.
- Remote Worker update action gated by backend safety. Done for manual latest
  updates from Worker cards.
- Update job progress and failure message.
- Web Control refresh notification when `/api/version` changes. Done.

Control TUI should show:

- Hub version and current client version.
- A startup warning when the client is older than the Hub's suggested version.
- `agentmux update apply --role control` as the recovery command.

## Security Model

Minimum requirements:

- Download over HTTPS.
- Verify SHA-256 before extracting.
- Extract archives into a temporary directory.
- Reject archives that contain unexpected paths or multiple executable names.
- Preserve file permissions explicitly.
- Keep previous binary for rollback.
- Never update Hub or Worker silently.

Recommended production requirements:

- Signed release manifest.
- Pinned public key embedded in the binary.
- Explicit update channels: `stable`, `beta`, `nightly`.
- Tenant-level policy for remote Worker update.
- Audit events for check, stage, apply, restart, success, and failure.
- Optional custom release repository for private deployments.

## Phased Plan

### Phase 1: Harden Existing Install Flow

- Keep `/install.sh` role-aware. Done.
- Verify `.sha256` files after download. Done.
- Add `AGENTMUX_VERSION` and `AGENTMUX_REPO` examples to docs. Done.
- Add `agentmux version` output with version, commit, date, role, OS, and arch. Done.

### Phase 2: Local Updater

- Add `internal/updater`. Done.
- Add `agentmux update check`. Done.
- Add `agentmux update apply --role control|worker|hub`. Done for Unix-like systems; Windows apply still requires manual replacement after stopping the Hub service.
- Add rollback metadata under `~/.local/state/agentmux/update`. Done.
- Support local Worker restart through existing `workerservice.Restart`. Done with `--restart`.

### Phase 3: Web/TUI Visibility

- Add `/api/version`. Done.
- Add Worker software inventory in `/api/workers`. Done.
- Show update badges and copyable commands in Web Control.
- Warn stale Control TUI clients during login/startup.

### Phase 4: Remote Worker Update

- Add Worker update protocol messages. Done.
- Implement staged Worker update and service restart. Done for manual latest
  updates.
- Require confirmation for `pty` backend. Done.
- Mark success only after reconnect with the target version. Done for in-memory
  jobs.
- Add update job persistence.
- Add update progress polling in Web Control.
- Add cancel support for queued jobs.

### Phase 5: npx-Style Runner

- Add `agentmux run <role>@<version>`. Done for local cached execution.
- Add Hub-served `/run.sh` bootstrap. Done.
- Cache verified binaries by version and role. Done.
- Add cache cleanup: `agentmux cache prune`.

### Phase 6: Signed Releases and Policy

- Publish signed release manifests.
- Embed trusted public key.
- Add tenant update policies.
- Add maintenance windows and max parallel Worker updates.

## Recommended First Implementation

Start with Phase 1 and Phase 2:

1. Add build-time version variables.
2. Add `agentmux version`.
3. Make `/install.sh` verify checksums.
4. Add `agentmux update check`.
5. Add local `agentmux update apply` for Control and Worker.

That creates the `npx`-like operational foundation without immediately taking
on the full complexity of remote fleet update orchestration.
