# Roadmap

## Phase 1: Correct Per-Attach PTY

Use one independent tmux client per control attach.

- Worker starts a fresh PTY for each control connection.
- Worker runs `tmux attach-session -t <session>` in that PTY.
- Each attach has a unique `stream_id`.
- Hub routes terminal output by `stream_id`, not only by session id.
- Control input is raw and goes only to that stream's PTY.
- Control detach closes only that stream's tmux client, never the underlying tmux session.
- Control reports terminal size at attach time. Done in CLI control.
- Resize events propagate from control to worker PTY. Done in CLI control.

This keeps tmux as the source of truth and should behave closest to SSH into a
machine and running `tmux attach`.

## Phase 2: Browser Control

Add a browser control surface using xterm.js.

- Hub serves a dashboard.
- Browser uses xterm.js for rendering.
- Browser reports terminal size with fit addon.
- Browser sends raw input and resize events over WebSocket.
- Hub routes streams to worker.

This should become the preferred UI for phones and laptops.

![AgentMux Web Control workspace](assets/visuals/agentmux-4-web-control-workspace-v1.png)

## Phase 3: Signal Exchange And Tenant-Aware Auth

Replace prototype direct join-token auth with a bootstrap exchange model.

- Landing page mints `amx_sig_...` signals.
- Worker/control exchange signals through `POST /api/exchange`.
- Hub returns scoped `amx_cred_...` credentials.
- HTTP and WebSocket routes accept credentials, not raw signals.
- `--token` remains as a development/admin override.
- All issued credentials carry role, scope, tenant id, expiry, and device id.
- Anonymous signals create temporary tenants.
- Registered-user signals later attach to durable tenants.

Implementation order:

1. Add in-memory `SignalStore` and `CredentialStore`.
2. Add `POST /api/signals` and keep `/api/join-tokens` as a compatibility alias.
3. Add `POST /api/exchange`.
4. Update worker `--join` to exchange before opening `/ws/worker`.
5. Update control CLI `--join` to exchange before API/WS calls.
6. Update Web `/control?signal=...` to exchange in-browser and store the credential.
7. Generate scoped Direct Token share URLs for anonymous Web/TUI Control.
8. Add tests that raw signals cannot access normal API/WS routes.

## Phase 4: Persistence And Policy

- Move signal/credential/worker/session metadata from memory to SQLite.
- Add expiry cleanup.
- Add credential revocation.
- Add audit events for exchange, connect, create, attach, resize, input, kill.
- Add worker policies for command allowlist, cwd roots, and session limits.
- Add tenant-level limits for anonymous mode.

## Phase 5: Headless Terminal State

Add a server-side terminal emulator so reconnecting controls can restore current
screen state instead of only receiving future bytes.

Candidate approaches:

- `@xterm/headless` plus `@xterm/addon-serialize` sidecar.
- Pure Go ANSI parser and cell buffer.
- C/Rust terminal emulator through FFI or sidecar.

Expected benefits:

- clean reconnect
- deterministic screen snapshots
- better multi-control fanout
- session state inspection
- agent status extraction from screen buffer

This is more complex than byte streaming because terminal output is a stateful
protocol, not plain text.

## Phase 6: Direct-First Transport

Make Control-Worker P2P the preferred terminal data path, with Hub relay as the
automatic fallback.

Current implementation status:

- Web Control has a Relay/P2P preferred switch for new terminal streams.
- `control.open` carries `channel_mode`, and `terminal.mode` reports
  `channel_mode`, `channel_state`, and fallback reason.
- P2P preferred currently falls back to Hub relay intentionally. No WebRTC
  signaling or direct data channel is active yet.

- Worker advertises direct-mode capabilities over the existing Hub control
  channel.
- Control requests an attach grant from Hub before opening a terminal stream.
- Hub issues short-lived grants and exchanges direct-mode signaling messages.
- Control attempts direct transport first and falls back to relay on timeout,
  unsupported capability, policy denial, or negotiation failure.
- Web Control shows per-pane transport state outside the terminal buffer:
  negotiating, direct, fallback, relay, interrupted, and closed.
- Control exposes direct negotiation diagnostics: stream ID, grant ID, ICE
  state, selected candidate pair, fallback reason, and last interrupt reason.
- Hub, Worker, and Control emit structured logs keyed by `stream_id` and
  `grant_id` for every attach attempt.

Candidate implementation order:

1. Add direct-mode capability flags and attach-grant protocol types.
2. Add Hub signaling messages for offer, answer, candidates, and interrupts.
3. Add Control state machine and UI badges while still using relay.
4. Add WebRTC DataChannel direct transport for Web Control and Go Worker.
5. Add automatic relay fallback and direct negotiation timeout.
6. Add detailed correlation logs and tests for fallback and revocation.

![AgentMux relay versus direct mode](assets/visuals/agentmux-7-relay-vs-direct-mode-v1.png)

## Phase 7: AI Gateway And Config Control

Extend AgentMux from terminal control into agent runtime configuration.

Target capabilities:

- Hub-managed AI provider definitions, model aliases, routing rules, and
  redacted secrets.
- Control UI for provider profiles, config profiles, rollout rules, usage
  status, and audit events.
- Worker capability flags for local AI gateway and managed config apply.
- Worker-side local OpenAI-compatible gateway endpoint for managed sessions.
- Hub-to-Worker config rollout with revision IDs, dry-run validation, atomic
  writes, rollback metadata, and redacted logs.
- Optional Hub-hosted gateway mode for deployments that want centralized proxy
  traffic.

Implementation order:

1. Add data model for provider profiles, config profiles, secret references,
   rollout targets, and revisions.
2. Add Hub HTTP APIs gated to registered Control credentials; Direct Token mode
   must not manage secrets or config.
3. Add Worker hello capabilities such as `ai.gateway.local.v1` and
   `agent.config.apply.v1`.
4. Add `config.preview` and `config.apply` protocol messages with apply result
   events.
5. Implement Worker-side staging, atomic write, backup, rollback, and redacted
   reporting.
6. Add a local Worker gateway endpoint and inject `OPENAI_BASE_URL` /
   `OPENAI_API_KEY` into new managed sessions.
7. Add usage reporting and health status back to Hub.

The first useful milestone is distributed cc-switch-style config rollout. The
AI gateway can then reuse the same profile, rollout, audit, and Worker apply
machinery instead of becoming a separate one-off subsystem.
