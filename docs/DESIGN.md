# AgentMux Design

## Goal

AgentMux provides a shell-layer remote control plane for multiple coding-agent
sessions across machines. It should not depend on an agent CLI's own remote,
resume, or API capabilities.

## Roles

```text
control
  |
  | HTTPS / WSS
  v
hub
  ^
  | outbound WSS
  |
worker
  |
  | tmux list / new-session / capture-pane / send-keys
  v
tmux sessions
  |
  v
codex / claude / gemini / opencode / bash
```

### Hub

The hub is the only component that needs to be reachable by other machines.
It accepts worker outbound WebSocket connections and control API/WebSocket
connections, authenticates them, tracks live workers and sessions, and routes
control input to the worker that owns a session.

The hub should be deployed behind HTTPS. It can also serve TLS directly with a
certificate and key.

### Worker

The worker runs on every machine that should host agent sessions. It opens an
outbound WSS connection to the hub, so it does not need a public IP or inbound
firewall rules.

Worker owns the local tmux adapter:

- list live tmux sessions
- create new sessions in a working directory
- adopt already-running sessions by reporting them
- stop sessions
- capture session output
- send input to sessions

### Control

Control is a client. The first implementation is CLI based; a browser UI can use
the same HTTP and WebSocket API.

Control can list workers/sessions, create sessions, attach to output streams,
and send synchronized input.

## Agent Transparency

The only required invariant is:

> The agent must believe it is running in an ordinary shell inside tmux.

AgentMux never modifies agent binaries, never injects plugins, and never uses
agent-specific remote APIs. The worker either creates:

```bash
tmux new-session -d -s task-1 -c /repo 'codex'
```

or observes existing sessions using:

```bash
tmux list-sessions
tmux list-panes
ps
```

Input is delivered with `tmux send-keys`; output is read with
`tmux capture-pane`. A later version can add a PTY bridge around
`tmux attach-session`, but the tmux session remains the source of truth.

## First Implementation Boundary

This repository starts with a small, testable MVP:

- one static shared token
- worker outbound WebSocket
- control WebSocket attach
- HTTP APIs for list/create/input/stop
- tmux capture polling for output streaming
- no database; hub state is in memory
- no transcript parser
- no file explorer

The architecture leaves room for:

- mTLS worker identity
- persistent SQLite state
- browser dashboard
- session recording
- RBAC
- direct PTY bridge

