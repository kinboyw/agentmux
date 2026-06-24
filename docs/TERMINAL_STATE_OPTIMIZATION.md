# Terminal State Optimization Plan

## Context

AgentMux currently streams terminal bytes from Worker to Control. In tmux mode,
each attach starts an independent PTY running:

```text
tmux attach-session -t <session>
```

This keeps tmux as the source of truth and gives strong compatibility with real
terminal applications, but it also means Control sees whatever a fresh tmux
client emits. On attach, tmux paints the current visible screen. On resize, tmux
and the application inside it may repaint the full visible area. Control does
not have a semantic terminal model; it only writes incoming bytes into xterm.js.

The resulting behavior is close to SSH over a high-latency link:

- Attach latency includes the initial tmux screen paint.
- Resize can produce full-screen repaint traffic.
- Reconnect cannot restore screen state without waiting for remote output.
- Input is displayed only after it reaches Worker and the remote PTY echoes it.

The desired direction is to move terminal state ownership to Worker while keeping
Hub as the identity, policy, and routing layer.

## Goals

- Attach should restore the current screen from a snapshot, not replay a long
  byte stream.
- History should be served as transcript pages on demand.
- Live output should continue as incremental updates after a snapshot boundary.
- Resize should avoid unnecessary full stream resets and make repaint traffic
  explicit.
- Input should support lower-latency UX through prediction and acknowledgement,
  while preserving a remote-authoritative model.
- tmux compatibility should remain available during migration.

## Non-Goals

- Do not make Hub the terminal state owner.
- Do not require inbound connections to Worker.
- Do not remove raw PTY streaming until the state-model path is proven.
- Do not assume terminal output is plain text. ANSI state, cursor movement,
  alternate screen, attributes, and wrap behavior must be modeled.

## Current Flow

### Attach

1. Control opens `/ws/control`.
2. Control sends `control.open` with `session_id`, `stream_id`, and terminal
   size.
3. Hub validates tenant and worker policy.
4. Hub forwards `terminal.open` to Worker.
5. Worker opens a backend stream.
6. In tmux mode, Worker starts `tmux attach-session` in a PTY.
7. Worker reads bytes from the PTY and forwards `terminal.output`.
8. Control writes those bytes into xterm.js.

### Resize

1. Browser `ResizeObserver` triggers xterm fit.
2. Control sends `terminal.resize` when cols or rows changed.
3. Hub forwards the resize to Worker.
4. Worker resizes the stream PTY.
5. tmux or the foreground program repaints as needed.
6. Repaint bytes are forwarded as normal `terminal.output`.

### Input

1. xterm.js emits input data.
2. Control sends `control.input`.
3. Hub forwards `terminal.input`.
4. Worker writes the bytes into the PTY.
5. The remote shell or app echoes output.
6. Control displays the echoed output.

There is no local echo in the current design.

## Target Architecture

Worker should maintain a terminal state model per session:

```text
PTY/tmux output
  -> Worker terminal parser
  -> screen buffer + modes + cursor + transcript
  -> snapshot/diff/history protocol
  -> Hub relay
  -> Control renderer
```

Worker remains the authority for terminal state. Hub relays state messages and
enforces authorization. Control renders snapshots and applies diffs. Raw byte
streaming remains as a compatibility fallback.

## State Model

Each Worker session should have a durable in-process `TerminalState` while the
Worker process is alive.

Suggested fields:

```text
session_id
generation
cols
rows
screen cells
cursor position and style
scroll region
active character attributes
terminal modes
alternate-screen flag
mouse modes
title
transcript ring
last_output_seq
last_input_seq
```

The state model should parse output from the actual PTY. It must not rely on
Control to interpret canonical state.

Candidate implementations:

- Go terminal emulator based on an existing ANSI parser.
- `@xterm/headless` with `@xterm/addon-serialize` as a Worker-side sidecar.
- A Rust/C terminal emulator exposed through a sidecar or FFI.

Near-term preference: start with a Go implementation if it can handle the common
escape sequence set well enough. Keep the parser behind an interface so a
sidecar can replace it if compatibility becomes the limiting factor.

## Snapshot And History Split

### Snapshot

A snapshot is the current terminal screen and enough metadata to render it
without replaying earlier bytes.

Example message:

```json
{
  "type": "terminal.snapshot",
  "session_id": "worker/demo",
  "stream_id": "web:...",
  "payload": {
    "generation": 42,
    "seq": 123456,
    "cols": 120,
    "rows": 36,
    "cursor": { "x": 12, "y": 35, "visible": true },
    "alternate": false,
    "title": "demo",
    "encoding": "cells-v1",
    "cells": "..."
  }
}
```

The first optimized attach should receive:

1. `terminal.snapshot`
2. live `terminal.diff` messages after the snapshot sequence

Control should not need historical transcript bytes to draw the current screen.

Rendering rule: Web Control should prefer xterm.js for the production visible
terminal. `cells-v1` is a state/debug format and should not become a long-term
replacement terminal renderer. When Worker-owned state needs to restore a Web
terminal, Worker should send a compact ANSI screen snapshot or replay boundary
that xterm.js can parse and render. Cell snapshots remain useful for debugging,
diff validation, non-browser controls, and future protocol checks.

### Final Render Mode Shape

The production Plan B target is `worker_state_xterm`, not a parallel React cell
renderer:

```text
Worker = terminal state authority
Control = attach/history/anchor coordinator
xterm.js = final visible terminal renderer
```

Render modes are mutually exclusive per pane:

- `worker_state_xterm`: Worker maintains canonical state and sends current ANSI
  snapshots plus live xterm-compatible updates. Control writes the current
  screen and live tail into xterm. History is fetched separately on demand.
- `live_attach_xterm`: compatibility fallback. Control attaches to a raw
  backend stream and writes raw output into xterm.

In `worker_state_xterm`, Worker should not also stream `cells-v1`/React-rendered
diffs by default. `cells-v1` is opt-in diagnostics. This avoids paying for both
xterm rendering and React cell rendering on the same live stream.

Attach and resize should be bounded by current screen size, not history length:

```text
attach -> terminal.snapshot(ansi-screen-v1) + live tail
resize -> terminal.state.reset + terminal.snapshot(ansi-screen-v1)
scroll up -> terminal.history.request/page
return bottom -> latest snapshot or buffered live tail alignment
```

Control should not force lazy history pages into xterm scrollback. xterm owns
the current terminal surface. Historical pages can be rendered in a virtual
history view and aligned to the xterm bottom by sequence numbers.

Worker-state mode has two separate planes:

- **View plane:** Control owns local layout, viewport size, scroll/history
  anchors, and rendering position. These local view changes must not implicitly
  mutate tmux/PTY state.
- **Command plane:** Control can still operate the remote tmux/PTY by sending
  explicit input or commands through Worker. Raw keystrokes, tmux prefix
  sequences, target selection, pane/window operations, and explicit size
  sync/reset are remote mutations and should update Worker canonical state,
  generation, and history boundaries.

The intended rollback behavior is Control-side viewport rollback, not Worker
terminal-state rollback. After `control.open`, Worker starts recording output for
the selected state domain, sends the current snapshot, and streams subsequent
terminal updates. If the user scrolls away from the live bottom, Control requests
older transcript pages with `terminal.history.request` and inserts those pages
into its history/view layer using `generation` and sequence anchors. Worker keeps
advancing the canonical current screen and does not rewind its emulator state to
serve historical pages. Returning to the bottom should realign Control to the
latest snapshot or buffered live tail.

`cells-v1` currently carries text, width, SGR attributes, foreground/background
color references, underline color, links, cursor position, and screen size.
ANSI palette colors are transported as `ansi:<index>` so Web Control can map
them through xterm.js' palette instead of baking a different palette into the
Worker. Truecolor values are transported as `#rrggbb`.

### History Transcript

Transcript is append-only historical text suitable for scrollback, search, and
inspection. It is not the source of truth for the visible screen.

Example request:

```json
{
  "type": "terminal.history.request",
  "session_id": "worker/demo",
  "payload": {
    "before_seq": 123456,
    "limit_lines": 500
  }
}
```

Example response:

```json
{
  "type": "terminal.history.page",
  "session_id": "worker/demo",
  "payload": {
    "start_seq": 120000,
    "end_seq": 123000,
    "lines": []
  }
}
```

Transcript generation should be conservative. Full-screen programs, alternate
screen content, progress bars, carriage-return rewrites, and cursor-addressed UI
should not be flattened naively into misleading logs.

## Live Updates

The live path should move from raw `terminal.output` bytes to structured updates:

```json
{
  "type": "terminal.diff",
  "session_id": "worker/demo",
  "payload": {
    "generation": 42,
    "from_seq": 123456,
    "to_seq": 123460,
    "ops": []
  }
}
```

Diff operations can start simple:

- set cell range
- clear line or region
- scroll region
- move cursor
- set title
- set mode
- reset screen

If diff generation becomes too complex, Worker can send a fresh snapshot after
large changes or parser uncertainty.

## Resize Strategy

Resize should become explicit state transition, not just PTY resize traffic.

### Current Coupling

The current attach path couples Control viewport size to the real Worker-side
terminal size:

```text
Control xterm fit
  -> terminal.resize
  -> Hub relay
  -> Worker stream.Resize(cols, rows)
  -> backend terminal size changes
```

For the tmux backend, Worker opens a live tmux client stream. Resizing that
stream can affect tmux client/window layout and can trigger a remote repaint.
For the built-in PTY backend, Worker owns the PTY and `Resize` changes the PTY
winsize directly. Foreground applications see `SIGWINCH` or equivalent terminal
size changes and may repaint.

This is correct for a raw attach model, but it means Control layout operations
such as sidebar toggles, split changes, browser zoom, and tab visibility can
have remote side effects. The same session may look unstable when several
Control panes or devices compete to resize it.

Recommended behavior:

1. Control reports proposed cols/rows.
2. Worker resizes the PTY or tmux client.
3. Worker increments `generation` if layout invalidates current screen state.
4. Worker sends `terminal.snapshot` for the new size after the remote repaint
   settles or after a short debounce window.
5. Worker resumes diffs from the new generation.

Short-term improvements before the state model:

- Debounce browser resize events.
- Ignore transient dimensions below a minimum useful size.
- Avoid remounting xterm when only pane focus changes.
- Keep sidebar and split layout changes from causing repeated cols/rows
  oscillation.

### Decoupled Resize With Worker State

The Worker-side state model should separate three dimensions:

- `process_size`: the real PTY or tmux client size observed by the remote
  process.
- `state_size`: the canonical terminal buffer size maintained by Worker.
- `viewport_size`: the Control-side rendering size.

Control resize should normally change only `viewport_size`. Worker may crop,
pad, or reflow the rendered snapshot for that viewer without changing the
remote process. Only explicit policy should change `process_size`.

Suggested policy:

```text
resize_policy = follow_control | fixed | worker_default
```

- `follow_control`: current behavior; useful for exact interactive attach.
- `fixed`: Worker keeps a configured canonical size, such as 120x36 or
  160x48. Control resize does not resize the real PTY/tmux client.
- `worker_default`: Worker chooses a stable size per backend and device class.

For the built-in PTY backend, `fixed` can be implemented by creating the PTY at
a canonical size and ignoring normal viewer resize messages. The foreground
program sees a stable terminal size. Control renders a viewport over Worker
state.

For the tmux backend, `fixed` requires more care because tmux has its own
client/window sizing semantics. A Worker-owned headless tmux client or
control-mode client can act as the canonical state feed. Viewer resize should
not create competing tmux clients that resize the same window. This is the
direction most aligned with multi-Control usage.

Open questions:

- Should fixed size be configured per Worker, per session, or per backend?
- Should interactive full-screen apps default to `follow_control` until the
  state parser is mature?
- How should multiple active Controls negotiate when a session is still in raw
  attach mode?
- Should Control show a warning when its viewport differs from the canonical
  process size?

## Input Latency Plan

With Worker-side terminal state, input can become sequence-aware:

```json
{
  "type": "control.input",
  "session_id": "worker/demo",
  "payload": {
    "input_seq": 99,
    "data": "a"
  }
}
```

Worker replies implicitly through terminal diffs and can optionally send:

```json
{
  "type": "terminal.input_ack",
  "payload": {
    "input_seq": 99,
    "accepted": true,
    "output_seq": 123461
  }
}
```

Control can then support optimistic input for safe cases:

- printable single-width characters
- normal screen only
- no active composition
- no mouse mode
- no bracketed paste in progress
- no known password/silent-input mode
- cursor position and line mode are predictable

The remote state remains authoritative. If Worker diffs disagree with the local
prediction, Control discards the prediction and applies Worker state.

This should make simple shell typing feel local while avoiding unsafe prediction
inside editors, TUIs, password prompts, and complex terminal modes.

## Protocol Compatibility

Keep the existing raw stream protocol during migration:

- `control.open`
- `control.input`
- `terminal.output`
- `terminal.resize`
- `terminal.close`

Add capability negotiation:

```json
{
  "type": "control.open",
  "payload": {
    "cols": 120,
    "rows": 36,
    "capabilities": [
      "terminal.snapshot.v1",
      "terminal.diff.v1",
      "terminal.history.v1",
      "terminal.input_ack.v1"
    ]
  }
}
```

Worker selects a mode:

```json
{
  "type": "terminal.mode",
  "payload": {
    "mode": "state-v1"
  }
}
```

Fallback modes:

- `raw-pty`: current behavior.
- `snapshot-only`: initial snapshot plus raw output.
- `state-v1`: snapshot, diffs, history, and input ack.

Current implementation status:

- Web Control sends `terminal.snapshot.v1` in `control.open.capabilities`.
- Worker replies with `terminal.mode` and a `terminal.snapshot` before opening
  the raw backend stream when that capability is requested.
- The current snapshot source is the existing backend `Capture` path with
  `ansi-lines-v1` encoding. This is a fast migration bridge, not the final
  terminal state model.
- Raw `terminal.output` remains authoritative after the snapshot. CLI/TUI
  controls do not request snapshot mode yet, so they stay on the original raw
  attach protocol.

Next PlanB milestones:

- Replace capture-backed snapshots with a Worker-side parser-backed
  `TerminalState`.
- Add `terminal.diff` generation after the snapshot boundary.
- Add history pagination separate from the visible screen snapshot.
- Add resize policy negotiation: `follow_control`, `fixed`, and
  `worker_default`.

## Create Session CWD Model

The current Create Session flow asks Control users to type `cwd` manually. This
is acceptable for a developer prototype, but it does not scale to many Workers,
many projects, or team usage. It also mixes two concerns:

- Control UX: helping the user choose a useful working directory.
- Worker authority: deciding what paths are allowed and executable.

Worker must remain the authority for local filesystem access. Hub and Control
should not infer filesystem permissions from paths alone.

### Product Interaction

Recommended interaction for Web Control:

1. User chooses a Worker.
2. Control shows known workspace roots for that Worker.
3. User selects a root or recent directory.
4. Control optionally supports path search/autocomplete inside allowed roots.
5. User chooses command or agent preset.
6. Worker validates `cwd` again when creating the session.

Good defaults:

- Remember recent CWDs per Worker and per user.
- Show project-like directories first, such as git repositories.
- Offer quick chips: home, last used, configured roots, recent repos.
- Keep manual path input available behind an "advanced path" affordance.
- Show whether the path is `verified`, `not found`, `not allowed`, or
  `unknown until create`.

### Permission Model

Do not let Control browse the entire Worker filesystem by default. Browsing and
completion should be scoped by Worker policy.

Suggested Worker configuration:

```json
{
  "workspace_roots": [
    { "id": "home-src", "path": "~/go/src", "label": "Go source" },
    { "id": "projects", "path": "~/projects", "label": "Projects" }
  ],
  "allow_manual_cwd": true,
  "manual_cwd_policy": "within_roots",
  "recent_cwd_limit": 20
}
```

Policy options:

- `within_roots`: manual paths must resolve under configured roots.
- `home_only`: manual paths must resolve under the Worker user's home.
- `any_local_path`: advanced self-hosted mode; Worker still validates path
  existence and executable permissions.
- `disabled`: only configured roots and discovered projects can be used.

Path validation should happen on Worker:

1. Expand `~` and environment-safe aliases only if Worker policy allows them.
2. Resolve symlinks with `EvalSymlinks` where practical.
3. Reject paths that escape allowed roots after resolution.
4. Check that the directory exists.
5. Check that the Worker process user can read and execute the directory.
6. Return a structured error instead of shelling out.

Hub should enforce tenant and device policy, but it should not inspect the
Worker filesystem directly.

### Proposed Protocol

Worker capability snapshot:

```json
{
  "type": "worker.capabilities",
  "payload": {
    "cwd": {
      "browse": true,
      "manual": true,
      "manual_policy": "within_roots",
      "roots": [
        { "id": "projects", "label": "Projects", "path": "~/projects" }
      ]
    }
  }
}
```

Directory listing request:

```json
{
  "type": "worker.cwd.list",
  "worker_id": "mywsl",
  "payload": {
    "root_id": "projects",
    "path": "agentmux",
    "limit": 100
  }
}
```

Directory listing response:

```json
{
  "type": "worker.cwd.entries",
  "worker_id": "mywsl",
  "payload": {
    "root_id": "projects",
    "path": "agentmux",
    "entries": [
      { "name": "cmd", "kind": "dir" },
      { "name": ".git", "kind": "dir", "hidden": true }
    ]
  }
}
```

Path validation request:

```json
{
  "type": "worker.cwd.validate",
  "worker_id": "mywsl",
  "payload": {
    "cwd": "~/projects/agentmux"
  }
}
```

Validation response:

```json
{
  "type": "worker.cwd.validation",
  "worker_id": "mywsl",
  "payload": {
    "cwd": "~/projects/agentmux",
    "resolved": "/home/kinboy/projects/agentmux",
    "status": "verified"
  }
}
```

### Security Notes

- Treat directory browsing as a privileged capability. It reveals project names
  and local filesystem structure.
- Do not expose hidden files by default; allow an explicit toggle.
- Never return file contents from CWD browsing endpoints.
- Rate-limit listing and search requests.
- Audit session creation with `worker_id`, `user_id`, requested `cwd`,
  resolved `cwd`, command, and policy result.
- Avoid shell interpolation in Worker validation and listing code.
- Prefer structured filesystem APIs over invoking `ls`, `find`, or shell
  commands.

### Migration Path

Phase 1:

- Keep manual `cwd`.
- Add recent CWDs per Worker in localStorage.
- Validate path on create and show Worker-returned errors clearly.

Phase 2:

- Add Worker workspace roots in config.
- Add Web Control root selector and recent directory chips.
- Keep manual input as advanced mode.

Phase 3:

- Add Worker directory browse and path validation APIs.
- Add typeahead/autocomplete within allowed roots.
- Persist recent paths in user settings after login.

Phase 4:

- Add project discovery, such as git repository indexing under configured
  roots.
- Add command presets per project or Worker.
- Add tenant/admin policy for whether manual paths are allowed.

## PlanB Decision: Worker-Owned State With Control Viewports

The Worker-side state path should support two terminal modes:

- `state`: Worker owns the terminal state, screen size, scrollback pages, and
  generation boundaries.
- `attach`: Worker opens a raw backend stream and Control renders bytes directly.

`auto` should be the default. In `auto`, Control requests state mode first and
Worker may degrade to attach mode when the backend, parser, or target does not
support state safely.

### Default Scope Policy

The default Web Control action should attach the whole tmux session. This keeps
the native tmux mirror semantics intact: status line, window switching, pane
navigation, prefix shortcuts, copy mode, mouse mode, and remote layout changes
all behave like a normal `tmux attach-session` client.

This default can resize the remote tmux client. That side effect is acceptable
for the primary remote-control workflow because preserving the user's existing
tmux operating model is more valuable than making the browser viewport fully
independent.

Window and pane targets are explicit fine-grained entries. They use the Worker
state pipeline:

```text
snapshot -> live terminal.output append -> history.request/page
```

Control still renders the final output with xterm. Worker and Hub prepare,
cache, route, and page the data; they do not replace xterm as the terminal
renderer. Pane targets should avoid mutating the remote tmux size where the
backend can provide live output without a real attached tmux client. Window
targets may start as composed snapshots/previews and later grow into a fuller
stateful target model.

### Size Model

PlanB uses three size concepts, but only two should drive remote output:

```text
process_size       tmux/pty size observed by the remote shell or TUI
worker_state_size  canonical Worker screen size
control_viewport   local browser/TUI pane size
```

For explicit Worker-state targets, `process_size` and `worker_state_size` should
stay equal. The remote program sees the canonical Worker size, for example
`120x36`, and produces output for that shape. `control_viewport` is only a
viewing window over that canonical screen. Control should crop, pad, and scroll;
it should not reflow remote terminal output.

This is the core state-isolation rule: Worker owns canonical terminal state and
remote size, while each Control owns only its local viewport, scroll anchor,
history cache, and rendering position. Opening, resizing, splitting, hiding, or
rotating a Control pane must not mutate the remote terminal size by default. A
remote size mutation is allowed only through explicit state operations such as
`terminal.size.sync` or `terminal.size.reset`.

State isolation does not remove remote control. It only prevents accidental
remote mutations caused by local layout. User intent still flows through the
command plane: terminal input, tmux prefix shortcuts, pane/window selection,
copy-mode keys, split/resize commands, and explicit sync/reset are all allowed
to change the remote session. Worker remains the authority that records the
resulting screen, transcript, and generation changes.

For the default whole-session attach, the backend may use a real tmux client.
In that mode the browser viewport can drive the attached tmux client size, and
the remote session may repaint accordingly. That is the expected compatibility
mode, not a PlanB state-mode failure.

This preserves ASCII diagrams, tables, progress bars, and full-screen TUIs. A
small Control viewport sees a clipped window into the canonical remote screen.
A large Control viewport sees the canonical screen bottom-aligned with padding.

### Session, Window, And Pane Scope

Worker-owned state must not mean one global state per session. A tmux session can
contain multiple windows and panes. On small screens, especially mobile TUI,
Control should use the remote pane as the minimum attach unit instead of trying
to render the whole remote split layout.

The ownership model is:

```text
session lifecycle: create/kill/list, credentials, high-level metadata
state domain:      session + target(window/pane) + generation
viewport:          per Control pane or mobile tab
```

For tmux, the stable state key should be built from the target identity:

```text
worker_id/session_name/window_id/pane_id
```

If a Control attaches to the whole tmux session, Worker should preserve native
session mirror semantics by default. If Control attaches to an explicit window
or pane target, Worker state should be keyed by that target. Pane-level targets
should remain independent. Mobile Control should avoid rendering multiple remote
panes side by side or stacked in the same terminal viewport because that shrinks
already constrained content and changes the user's mental model of the remote
pane. Instead, it should support
multiple pane attachments as switchable focus targets:

- one tab per pane,
- a focused pane plus preview cards,
- a pane switcher opened by shortcut,
- previous/next pane shortcuts,
- optional worker/session/window/pane breadcrumb in the status line,

without every pane fighting over the same cursor, size, mouse mode, or history
anchor.

For mobile TUI, the interaction model should be closer to Vim's help pages than
to a desktop dashboard:

- `?` opens a full-screen help/usage page.
- `q` closes help or exits the current overlay.
- `Tab` / `Shift-Tab` switches attached panes.
- `[` / `]` can switch previous/next remote pane when tab keys are unavailable.
- `/` enters search/filter mode for worker, session, or pane lists.
- `Enter` focuses or attaches the selected pane.
- `Esc` returns to the previous mode.

The help page should show the current mode, available commands, and short usage
examples. It should be generated from the same keybinding registry used by the
TUI so documentation and behavior cannot drift.

The whole-session mirror is the default attach mode for maximum compatibility.
PlanB state mode remains available for explicit pane/window targets because
they are deterministic and compose cleanly on small screens.

### Control TUI Layout Policy

Control TUI should adapt by terminal size and input device:

```text
desktop TUI: overview, pane list, optional split workspace, command palette
mobile TUI:  focused pane, tab strip or compact switcher, help overlay
```

The mobile policy is intentionally conservative:

- never auto-render a remote multi-pane tmux window as multiple local panes;
- never shrink a pane below a readable width just to preserve remote layout;
- prefer one remote pane per local view;
- keep multiple attached panes alive in the background and switch focus without
  re-attaching when possible;
- use preview cards/lists for discovery, not for live side-by-side operation.

This keeps Worker state compatible with both Web Workspace and mobile TUI. Web
can still compose multiple pane targets in a large viewport, while mobile uses
the same target states through tabs and keyboard navigation.

### Sync And Reset

Control may explicitly mutate the remote size through a paired operation set:

- `sync`: make the remote size equal to the current Control viewport.
- `reset`: restore the remote size to the Worker default canonical size.

These actions are intentionally explicit. Normal browser pane changes, sidebar
toggles, tab visibility changes, and split drags should not resize tmux/pty.
That keeps multi-Control usage stable and avoids accidental remote repaints.

Both `sync` and `reset` are remote-size mutations. They must:

1. resize the underlying tmux/pty process,
2. set `worker_state_size` to the same size,
3. increment `generation`,
4. emit `terminal.state.reset`,
5. emit a fresh `terminal.snapshot`,
6. resume diffs for the new generation.

The UI should present them as a pair:

```text
State · remote 120x36 · viewport 96x31
[Sync to viewport] [Reset to default]
```

When multiple Controls are attached, `sync` should be treated as a visible
remote mutation because it affects every viewer.

### Generation Rule

The current screen state is valid only within one generation:

```text
same generation: snapshot + diff can be applied
new generation: discard old pending diffs and wait for the next snapshot
history: may cross generations with resize boundary markers
```

`terminal.state.reset` is the boundary event. Control should clear the visible
screen, preserve history anchors when the user is scrolled up, and wait for the
next snapshot. If the user is at bottom, the next snapshot should be
bottom-aligned automatically.

### On-Demand History

Control must not eagerly fetch all history. It should keep:

- the current screen,
- live diffs after the current snapshot,
- only history pages explicitly requested by user scrolling.

Older scrollback is requested with `terminal.history.request` and returned as
`terminal.history.page`. Pages should include sequence ranges and a `has_more`
flag. Resize events should be represented as history boundary markers instead
of clearing transcript history.

History pages are transcript pages, not terminal-emulator checkpoints. They may
cross generation boundaries and contain resize markers, but they should be
rendered as historical content anchored relative to the live screen. Control is
responsible for preserving the user's scroll position while prepending older
pages, for avoiding duplicate rows at page boundaries, and for bottom-aligning
back to the live stream when the user exits rollback/scrollback view.

### Initial Implementation Slice

The first PlanB implementation should be a conservative skeleton:

1. Add protocol fields and messages for terminal mode, state reset, history
   request/page, and size sync/reset.
2. Add Worker config for terminal mode and default state size.
3. Let Control request `transport_mode: auto` and show the negotiated mode.
4. Keep raw attach as the production fallback.
5. Implement `sync` and `reset` as explicit remote-size commands even before
   full cell diffs exist.
6. Continue using the existing `terminal.snapshot` + raw stream bridge until a
   real `cells-v1` state engine lands.

This gives users a visible model and gives the codebase stable protocol seams
without betting the whole terminal experience on a new parser in one step.

Current implementation status:

- `transport_mode: auto`, `terminal.mode`, `terminal.state.reset`, `sync`, and
  `reset` are wired through Hub, Worker, and Web Control.
- Web Control opens state-capable streams by default and stops sending ordinary
  viewport resize messages once Worker negotiates `worker_state`.
- Web Control updates only `control_viewport` on normal layout changes in
  `worker_state`; `terminal.size.sync` and `terminal.size.reset` are explicit
  command-plane actions or narrowly scoped recovery operations.
- Worker keeps a bounded transcript ring and can answer
  `terminal.history.request` with `terminal.history.page`.
- Worker also maintains a `cells-v1` screen snapshot through the terminal
  parser, but Web Control should request cell snapshots only when diagnostics are
  explicitly enabled. The production path is `worker_state_xterm`: xterm.js
  receives ANSI snapshots plus live output, while cell snapshots/diffs remain
  opt-in diagnostics.
- Web Control can still expose Worker-side cells as an opt-in debug surface in a
  later iteration. It should not subscribe to cell snapshots on every pane by
  default.
- The runtime is still a bridge until a session-level Worker state feed replaces
  per-Control tmux attach. The immediate performance win is removing parallel
  cells/diff rendering overhead while preserving xterm as the final renderer.

## Implementation Phases

### Phase 1: Instrument Current Behavior

- Add debug counters for attach initial bytes, resize events, resize output
  bytes, input round-trip time, and reconnect time to first paint.
- Add optional tracing around `control.open`, `terminal.open`,
  `terminal.resize`, `terminal.input`, and first `terminal.output`.
- Use the metrics to validate later improvements.

### Phase 2: Control-Side Resize Hygiene

- Debounce `ResizeObserver` events.
- Coalesce repeated fits in the same animation frame.
- Send resize only after dimensions are stable for a short window.
- Add tests for pane split/sidebar changes if practical.

This phase reduces avoidable repaint traffic without changing architecture.

### Phase 3: Worker Terminal Parser Prototype

- Introduce `internal/terminalstate`.
- Feed PTY output into both raw stream and terminal state parser.
- Expose an internal snapshot API.
- Compare snapshot rendering with xterm.js for common shell output.
- Keep production Control on raw output.

### Phase 4: Snapshot Attach

- Add `terminal.snapshot` protocol.
- On optimized attach, Worker sends current snapshot first.
- Control renders the snapshot into xterm.js or a custom cell renderer.
- Continue raw `terminal.output` after snapshot as a temporary bridge.

This phase proves fast attach before solving every diff edge case.

### Phase 5: History Transcript API

- Add Worker transcript ring.
- Add `terminal.history.request` and `terminal.history.page`.
- Stop using initial raw replay as scrollback.
- Let Control request older transcript pages when the user scrolls near the top.

Status: protocol and Worker ring/page response exist. Control-side scroll
triggering and history prepend rendering are still pending.

### Phase 6: Structured Diffs

- Add `terminal.diff`.
- Apply diffs in Control after snapshot.
- Send a full snapshot when diff sync is lost or generation changes.
- Keep raw stream fallback by capability.

Status: Worker emits conservative row-level `terminal.diff` messages after an
initial `cells-v1` snapshot. Web Control applies `replace_row` and `cursor`
ops when generation matches. If size or generation state is not compatible,
Control waits for the next full snapshot. Cell diffs now preserve SGR color
metadata, but they are still treated as diagnostics rather than the preferred
production renderer.

### Phase 7: Input Ack And Optimistic Echo

- Add `input_seq` and `terminal.input_ack`.
- Measure input RTT.
- Implement conservative optimistic echo.
- Disable prediction automatically in unsafe modes.
- Add visible correction only if absolutely necessary; prefer seamless rollback.

### Phase 8: tmux Integration Options

Evaluate two tmux paths:

1. Continue one tmux attach client per Control stream, but parse its output in
   Worker.
2. Use a single headless tmux client per session as the Worker-owned state feed,
   then fan out state to multiple controls.

The second option is more aligned with Worker-side ownership, but it must be
tested carefully with tmux session size semantics and multi-client behavior.

## Risks

- Terminal emulation correctness is hard. Editors and TUIs will expose parser
  gaps quickly.
- Transcript is not equivalent to terminal history. Flattening screen UIs can
  produce confusing logs.
- Optimistic echo can leak characters in password prompts if mode detection is
  wrong.
- tmux has its own client/window sizing semantics. A Worker-owned headless
  client may influence layout unless isolated carefully.
- Persisting state across Worker restarts is a separate problem. This plan only
  targets live Worker process state first.

## Acceptance Criteria

- Reattaching to an idle shell paints from snapshot without replaying large raw
  output.
- Scrolling upward requests transcript pages on demand.
- Resizing produces at most one post-resize snapshot per stable size.
- Simple shell typing can render optimistically with automatic correction.
- vim, tmux prefix handling, password prompts, and alternate-screen programs
  remain remote-authoritative and correct.
- Raw streaming remains available as a fallback.
