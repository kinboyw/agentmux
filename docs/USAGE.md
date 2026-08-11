# AgentMux Usage Guide

This guide covers the normal operating path for AgentMux: run Hub with a stable
public URL, join Workers with a short-lived signal, and control long-running
terminal sessions from the browser or TUI.

## Concepts

AgentMux has three roles in one binary:

- `hub`: the HTTPS/WSS entrypoint, API server, Web Control host, and session router.
- `worker`: an outbound connector that manages local tmux or built-in PTY sessions on a machine where agents run.
- `control`: CLI commands for listing, creating, and attaching to sessions. The browser control surface is served by Hub at `/control`.

The agent itself remains unaware. Codex, Claude, Gemini, OpenCode, or a shell
runs inside a local terminal backend; Worker attaches below it at the terminal
layer.

## Quick Start

If you already have an AgentMux Hub URL, open that landing page and use the
generated commands:

1. Click `Generate join signal`.
2. Run the generated Worker command on the machine that owns your sessions.
3. Open the generated Web Control share URL, or copy the Direct Token into Web
   Control / TUI.

That is the normal user path. Hub deployment is only needed when you want to run
your own private Hub.

## Local Smoke Test

For development, run the tmux smoke script:

```bash
./scripts/dev-tmux.sh
```

Open:

```text
http://127.0.0.1:8081/control?token=dev-token
```

This path uses local source builds and is meant for development only.

## Run Hub From A Release Binary

Install the binary from GitHub Releases, then start a self-hosted Hub:

```bash
agentmux hub \
  --addr 0.0.0.0:8081 \
  --data ./agentmux.db \
  --public-url https://hub.example.com
```

Important flags:

- `--addr`: local listen address.
- `--data`: SQLite database path for users, signals, and credentials.
- `--public-url`: external HTTPS URL used to generate worker/control commands.
  Required behind a reverse proxy or tunnel because forwarded headers are
  intentionally not trusted.
- `--release-repo`: GitHub `owner/repo` for `/install.sh` downloads. Defaults to `kinboyw/agentmux`.

On Windows, the release currently supports the Hub role. A convenience starter
is available in the repository under `scripts\run.bat`; edit `PUBLIC_URL`,
`ADDR`, or `DATA` in that file before starting a production Hub.

## Run Hub From Docker Image

Release tags publish a multi-arch image to GitHub Container Registry:

```bash
docker pull ghcr.io/kinboyw/agentmux:latest
```

Run a self-hosted Hub:

```bash
docker run -d --name agentmux --restart unless-stopped -p 8081:8081 ghcr.io/kinboyw/agentmux:latest
```

The image defaults to `hub --addr 0.0.0.0:8081 --data /var/lib/agentmux/agentmux.db`.
Use `-v agentmux-data:/var/lib/agentmux` when you want the embedded database to
survive container replacement.

For local compose-based development:

```bash
cp deploy/agentmux.env.example .env
docker compose up -d --build
```

## Put Hub Behind Cloudflare Tunnel

There are two different Cloudflare Tunnel modes. The `--public-url` value must
match the URL that users, Workers, and Controls will actually use.

### Named tunnel with your own domain

Use this when you already know the public hostname, for example
`https://hub.example.com`. This is the production path.

Start Hub locally:

```bash
agentmux hub \
  --addr 127.0.0.1:8080 \
  --data /var/lib/agentmux/agentmux.db \
  --public-url https://hub.example.com
```

Start the named tunnel:

```bash
cloudflared tunnel run agentmux
```

Cloudflare terminates HTTPS and forwards WebSocket upgrades to Hub. Workers
still connect outbound-only over `wss://...`.

### Quick tunnel with a generated trycloudflare.com URL

For a temporary tunnel, you do not know `--public-url` until `cloudflared`
prints it.

First start Hub with a local URL:

```bash
agentmux hub \
  --addr 127.0.0.1:8080 \
  --data /var/lib/agentmux/agentmux.db \
  --public-url http://127.0.0.1:8080
```

Then run:

```bash
cloudflared tunnel --url http://127.0.0.1:8080
```

Copy the printed `https://*.trycloudflare.com` URL, stop Hub, and restart Hub
with that value:

```bash
agentmux hub \
  --addr 127.0.0.1:8080 \
  --data /var/lib/agentmux/agentmux.db \
  --public-url https://example.trycloudflare.com
```

Then open that public URL and generate Worker/Control commands. Without this
restart, Hub will generate local-only commands.

## Join A Worker

Open the landing page:

```text
https://hub.example.com/
```

Click `Generate join signal`, then run the generated Worker command on the machine that owns the local sessions:

```bash
curl -fsSL https://hub.example.com/install.sh | sh -s -- worker --join 'amx_sig_...' --name "$(hostname)"
```

The script uses an existing `agentmux` in `PATH`, builds from source when run inside a checkout, or downloads the matching GitHub release archive.
Worker join is best-effort for recoverable failures: network errors, Hub 5xx,
HTTP 408, and HTTP 429 are logged and retried with backoff until the signal
exchange succeeds or the process is interrupted. Configuration and permission
errors such as an invalid Hub URL, wrong tenant, or unauthorized signal fail
fast.

A Worker keeps a stable local instance id. Reusing the same signal from the same
Worker instance is rejected, which prevents accidental duplicate joins caused by
changing only `--name`. A Worker also refuses to join a different Hub while an
existing Worker credential is configured. Leave the current Hub first:

```bash
agentmux worker leave
agentmux worker join --hub https://hub.example.com --join 'amx_sig_...' --name "$(hostname)"
```

`worker leave` stops the local background Worker by default and clears the saved
Worker credential for the configured Hub/id.

Useful local service commands:

```bash
agentmux worker status
agentmux worker restart
agentmux worker logs -n 80
agentmux worker logs -f
```

`worker status` prints local configuration, credential presence, backend
resolution, log path, pid/lock metadata, and service-manager state. `logs -f`
follows `journalctl --user` when available and falls back to the local worker
log file.

### Worker session backend

Worker defaults to `auto` backend selection:

- `tmux` is used when available.
- If tmux is not available, Worker falls back to the built-in PTY backend and prints a warning.

Built-in PTY sessions can be detached and re-attached from Control while the
Worker process is alive, but they are not durable across Worker process stops or
restarts. Install tmux when you want local sessions to survive Worker restarts.

Configure the default backend:

```bash
agentmux worker config --backend auto
agentmux worker config --backend tmux
agentmux worker config --backend pty
```

You can also pass `--backend auto|tmux|pty` to `agentmux worker run`,
`agentmux worker join`, or `agentmux worker start`. Explicit `tmux` fails fast
when tmux is missing; `auto` falls back to built-in PTY.

## Open Web Control

Use the generated Web Control share URL:

```text
https://hub.example.com/control?token=amx_cred_...
```

The share URL carries a Direct Token. Direct Token mode is intentionally narrow:
it lists sessions for the shared tenant, connects to a selected session, and
lets you switch between sessions. It does not allow session creation, Worker
join signal generation, Worker management, previews, workspaces, version/update
prompts, or registered-account features.

Anonymous shares are lifecycle-bound to that Direct Token. After the Direct
Token expires, Hub evicts Workers registered under the anonymous tenant and
interrupts live anonymous Worker/Control connections so abandoned shares do not
accumulate runtime state.

Registered users should sign in from Web Control for the full management
surface: create/stop sessions, join Workers, manage Workers, use previews,
operate workspaces, and queue Worker updates.

## CLI Control

Install or bootstrap the Control/TUI client from a Hub:

```bash
curl -fsSL https://hub.example.com/install.sh | sh -s -- control
```

The installer downloads the matching `agentmux-tui` binary and starts the TUI
against that Hub. Newer releases publish real `agentmux-tui-*` artifacts; older
`agentmux-control-*` and legacy `agentmux-*` artifacts are only fallback paths.

With no local configuration, `agentmux-tui` uses the public AgentMux Hub:

```bash
agentmux-tui
```

Set or change the default Hub explicitly:

```bash
agentmux-tui --hub https://hub.example.com
```

The explicit `--hub` value is cached in the local AgentMux config. Later
`agentmux-tui`, `agentmux control app`, and `agentmux control login` use the
cached Hub unless `AGENTMUX_CONTROL_HUB`, `AGENTMUX_HUB`, or another `--hub`
value overrides it.

Inside the TUI:

- `/login` starts device login for the current default Hub, tries to open the
  local browser, and still shows the login URL and code for manual copy/open.
- `/hub` switches the current Hub and stores it as the new default. If a cached
  Control credential exists for that Hub, the TUI reconnects automatically;
  otherwise run `/login`.
- `/refresh` reloads the session list.

Useful source-development commands:

```bash
go run ./cmd/agentmux control list --hub http://127.0.0.1:8081 --token dev-token
go run ./cmd/agentmux control create --hub http://127.0.0.1:8081 --token dev-token --worker local --name demo --cwd "$PWD" --command bash
go run ./cmd/agentmux control attach --hub ws://127.0.0.1:8081 --token dev-token --session local/demo
```

Inside `control attach`, press `Ctrl-]` to detach from the local control stream. The worker-side tmux session keeps running.

For TUI development, enable the debug HUD and append-only log:

```bash
go run ./cmd/agentmux control app --hub http://127.0.0.1:8081 --token dev-token --debug --debug-log /tmp/agentmux-tui-debug.log
```

Inside the TUI, press `D` from the session list to append a JSON state snapshot to the debug log. Attached sessions run in the right-side terminal area by default; use `Ctrl-F` to toggle full-screen, `Ctrl-]` to detach, `Ctrl-Q` to quit the TUI, or `Ctrl-G` to write a debug snapshot. Normal keys are sent to the remote terminal. The snapshot records render counters, stream queue sizes, selected session metadata, and terminal view sizes, but not credentials or terminal output.

For runtime profiling, enable the optional pprof endpoint on Hub or Worker. Keep
the address bound to localhost unless the port is protected by a private
network.

```bash
agentmux worker run --pprof-addr 127.0.0.1:6060
agentmux hub --pprof-addr 127.0.0.1:6061

go tool pprof -http=:0 http://127.0.0.1:6060/debug/pprof/heap
go tool pprof -http=:0 http://127.0.0.1:6060/debug/pprof/profile?seconds=30
```

The same setting can be provided with `AGENTMUX_WORKER_PPROF_ADDR`,
`AGENTMUX_HUB_PPROF_ADDR`, or the shared `AGENTMUX_PPROF_ADDR` fallback.

## Version And Updates

Check the current binary:

```bash
agentmux version
agentmux version --json
```

Check for an available release and update an installed binary:

```bash
agentmux update check --role control
agentmux update apply --role control
agentmux update rollback
```

Worker can restart its local background service after the new binary is staged:

```bash
agentmux update apply --role worker --restart
```

From Web Control, use the `Update` action on a Worker card to queue the same
operation remotely. The action is enabled only for online Workers that advertise
the `worker.update.apply` capability. tmux-backed Workers can restart without
terminating tmux sessions. Built-in PTY Workers require an explicit disruptive
restart confirmation because in-process sessions may be lost.
When Hub has a concrete release version and a Worker reports an older version,
Web Control shows an update notice on the Worker card. After an update is
queued, the card tracks the in-memory job status such as `sent`, `started`,
`restarting`, `succeeded`, or `failed`.

Web Control also checks `/api/version` in the background. When Hub serves a new
Web Control build, the browser shows a refresh notification instead of silently
reloading an active terminal workspace.

For `npx`-style temporary Control usage, run from the verified cache instead of
mutating the installed binary:

```bash
agentmux run control@latest
curl -fsSL https://hub.example.com/run.sh | sh -s -- control@latest
agentmux cache prune
```

TUI Direct Token access is supported with the same boundary as Web Control
Direct Token mode:

```bash
agentmux-tui --hub https://hub.example.com --token 'amx_cred_...'
```

## Data And Backups

SQLite persists:

- anonymous join signals
- scoped credentials
- registered users
- browser/TUI device auth sessions
- registered Worker records and software inventory
- Worker update jobs and update events

Runtime state is rebuilt as workers reconnect:

- live Worker connections
- active WebSocket streams
- live session snapshots

Back up the configured `agentmux.db` file for Hub identity state.
