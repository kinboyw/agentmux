# AgentMux Business Roadmap

## Working Thesis

AgentMux should not become another coding agent, IDE, or model gateway. The more
durable opportunity is to become an AgentOps control plane for coding agents.

The product direction:

```text
AgentMux = secure runtime control plane for code agent sessions
```

AgentMux should manage where agents run, how humans attach to them, what they
did, who approved actions, how sessions recover, and how teams audit the work.

Model gateways manage model calls. AgentMux should manage agent execution sites.

## Market Context

Code agent usage is moving from a single developer running one CLI locally to
teams running many long-lived agents across laptops, dev boxes, CI runners, and
cloud machines.

This creates operational questions:

- Which agents are running right now?
- Which repo, branch, workspace, and command does each agent control?
- Who can observe, interrupt, approve, or take over a session?
- What commands did the agent run?
- What files changed?
- Which tests passed or failed?
- Which model, gateway, API key, and account did the agent use?
- How much did the session cost?
- Can the team replay or audit the session later?
- Can the company self-host the execution layer?

The current ecosystem is strong at agents and increasingly strong at model
routing. It is still early at agent runtime operations.

## Positioning

Preferred positioning:

```text
Tailscale + tmux + Datadog + Temporal for coding agents
```

More precise product statement:

```text
AgentMux is the control plane for running, observing, recovering, and auditing
AI coding agents across developer and server environments.
```

Avoid narrow positioning:

- Not "a Claude Code web UI".
- Not "a better tmux".
- Not "an AI gateway".
- Not "a replacement for Cursor, Claude Code, Codex, or Copilot".

Strong positioning:

- Bring your own agent.
- Bring your own model gateway.
- Run anywhere.
- Own your execution history.
- Keep humans in control.

## Relationship To AI Gateways

AI gateways such as Cloudflare AI Gateway, Vercel AI Gateway, OpenRouter, and
LiteLLM focus on:

- provider routing
- rate limiting
- caching
- model fallback
- request logging
- cost and usage visibility

AgentMux should integrate with this layer rather than compete with it.

Integration opportunities:

- Show the model gateway used by each agent session.
- Track `OPENAI_BASE_URL`, `ANTHROPIC_BASE_URL`, and provider metadata.
- Correlate terminal transcript with model usage logs.
- Attribute model cost to repo, branch, task, session, worker, and user.
- Let teams define per-session or per-worker model policies.
- Restart or migrate an agent session with a different provider profile.

The strategic split:

```text
AI Gateway: model-call control plane
AgentMux: agent-execution control plane
```

## Product Pillars

### 1. Agent Session Runtime

AgentMux should reliably start, attach, detach, reconnect, stop, and recover
long-lived code agent sessions.

Core features:

- Worker enrollment.
- Session creation.
- Web and CLI control surfaces.
- tmux and PTY backends.
- Multi-pane Web Control.
- Stable reconnect.
- Worker-side terminal state model.
- Snapshot-based attach.
- Transcript history on demand.

### 2. Agent Session Recorder

AgentMux should record what happened in an agent session in a way that is useful
for humans and organizations.

Record:

- terminal snapshots
- transcript pages
- user input
- shell commands
- process lifecycle
- file changes
- git branch and commit metadata
- test commands and results
- agent prompts if available
- approval decisions
- model gateway metadata

The long-term asset is not raw terminal bytes. It is the intent-to-action
timeline:

```text
human intent -> agent plan -> command -> file diff -> test -> review -> PR
```

### 3. Policy And Approval

AgentMux should make high-trust agent execution possible in team environments.

Policy examples:

- block or approve dangerous commands
- restrict cwd roots
- restrict file paths
- require approval before push or deploy
- redact protected environment variables
- limit command runtime
- enforce worker labels and tenant boundaries
- record audit events for sensitive actions

This is likely more valuable to companies than terminal UI polish.

### 4. Multi-Agent Workspace

AgentMux should support multiple agents working in parallel on one task or
across many tasks.

Examples:

- frontend agent
- backend agent
- test agent
- reviewer agent
- docs agent
- migration agent

Control plane responsibilities:

- session grouping
- task metadata
- shared progress view
- conflict visibility
- handoff between agents and humans
- final summary and PR packaging

### 5. Agent-Agnostic Integrations

AgentMux should work with many coding agents:

- Claude Code
- Codex CLI
- Gemini CLI
- Goose
- Aider
- Copilot CLI-style tools
- local model agents
- internal company agents

The runtime should treat agents as processes, but progressively add adapters for
better metadata extraction.

Adapter examples:

- identify agent binary and version
- detect current model
- detect base URL or gateway
- detect repo and branch
- parse known JSONL logs if available
- attach MCP server metadata if available

## Technical Bets

### Worker-Side Terminal State

This is the most important technical bet.

Without Worker-side state, AgentMux is mostly a remote shell relay. With
Worker-side state, AgentMux can provide:

- fast attach
- reconnect restore
- searchable transcript
- session replay
- structured diffs
- multi-control fanout
- input acknowledgement
- safer optimistic echo
- audit snapshots

The implementation should split:

```text
current screen snapshot != historical transcript != raw terminal bytes
```

### Structured Timeline

The terminal state model should feed a higher-level timeline:

- terminal events
- command events
- filesystem events
- git events
- model/gateway events
- approval events
- user takeover events

This timeline is the product's long-term moat.

### Agent-Agnostic Runtime

The system should not depend on one model vendor or one agent CLI.

Design principle:

```text
The Worker owns processes and terminal state. Agent-specific adapters add
metadata, but the system still works without them.
```

## Product Packaging

### Open Source Core

Keep the core useful and self-hostable:

- Hub
- Worker
- Web Control
- CLI Control
- tmux/PTY sessions
- basic auth
- basic snapshot/replay
- local SQLite
- single-user or small-team setup

Open source helps adoption because developers are skeptical of tools that sit
between their codebase and their agents.

### Commercial Edition

Commercial features should map to team and enterprise pain:

- SSO/SAML/OIDC
- RBAC
- team workspaces
- audit log retention
- encrypted transcript storage
- policy engine
- approval workflow
- gateway cost analytics
- centralized deployment management
- compliance exports
- hosted control plane
- enterprise support

### Pricing Hypotheses

Possible packaging:

- free local/self-hosted personal edition
- team self-hosted license
- hosted SaaS by seat and active worker
- enterprise by workspace, retention, and support tier

Avoid complex pricing before usage patterns are clear.

Initial commercial validation can be:

```text
Can a 5-20 person engineering team pay for secure shared agent sessions,
replay, approval, and audit?
```

## Competitive Risks

### Platform Absorption

Anthropic, OpenAI, GitHub, Cursor, or another major vendor may add team session
dashboards, replay, and remote execution.

Defense:

- stay agent-agnostic
- support self-hosting
- support mixed model gateways
- focus on execution history ownership
- integrate with enterprise policy and audit requirements

### Too Much Terminal, Not Enough AgentOps

If AgentMux remains only a better browser tmux, it will be useful but hard to
commercialize.

Defense:

- prioritize session state, replay, timeline, policy, and team workflows
- treat terminal UI as necessary infrastructure, not the final product

### Terminal Emulation Complexity

Terminal correctness is hard, especially for editors, TUIs, alternate screen,
mouse mode, and resize behavior.

Defense:

- keep raw stream fallback
- introduce snapshot first, then diffs
- disable optimistic echo in unsafe modes
- test against common tools like shells, vim, tmux, fzf, htop, and coding agents

## Near-Term Roadmap

### Stage 1: Reliable Control Plane

- stabilize Web Control attach/detach
- improve resize behavior
- preserve terminal panes across UI interactions
- add better worker/session status
- add debug counters for attach latency, output bytes, resize frequency, and
  input RTT

### Stage 2: Worker Terminal State

- implement Worker-side terminal parser prototype
- add terminal snapshot API
- split current screen snapshot from transcript
- add transcript paging
- add reconnect restore
- keep raw PTY fallback

### Stage 3: Session Replay And Search

- record session timeline
- search transcripts
- replay session output
- export session bundle
- associate session with repo, branch, cwd, command, and worker

### Stage 4: Agent Metadata

- detect common agent CLIs
- capture model/gateway environment
- capture git state
- capture command lifecycle
- display per-session metadata in Web Control

### Stage 5: Policy And Approval

- define policy DSL or structured policy config
- add dangerous command approval
- add path and cwd restrictions
- add user takeover and approval audit log
- add protected env redaction strategy

### Stage 6: Team Product

- team workspace
- RBAC
- SSO/OIDC
- shared session observation
- session comments or handoff notes
- audit retention

### Stage 7: Gateway And Cost Integration

- integrate with provider/gateway logs where possible
- attribute cost by session/task/repo/user
- show latency and error rates by provider
- support per-session gateway profile selection

## Validation Questions

Track these while developing:

- Do developers want to observe and take over each other's agent sessions?
- Is session replay valuable enough to change behavior?
- Which teams need approval before agent actions?
- What is the minimum useful audit trail?
- Does model cost attribution matter at the session level?
- Is self-hosting a requirement or a nice-to-have?
- Which agent integrations create the most pull?
- Can this become part of daily engineering workflow rather than an occasional
  debugging tool?

## Current Strategic Priority

The next major product bet should be:

```text
Worker-side terminal state -> snapshot attach -> transcript history -> replay
```

This unlocks the rest of the business direction. It turns AgentMux from a live
terminal relay into an agent runtime system with memory.

