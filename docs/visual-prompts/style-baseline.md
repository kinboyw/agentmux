# Style Baseline

## Visual Identity

AgentMux should feel like a serious infrastructure tool for power users:
technical, quiet, precise, and trustworthy. Avoid playful mascot-style imagery.

## Aesthetic Direction

- Modern technical editorial illustration.
- Dark neutral background with restrained accent colors.
- Palette:
  - near-black graphite: `#070909`
  - deep panel green-black: `#101414`
  - muted border graphite: `#273131`
  - terminal foreground: `#edf5f2`
  - AgentMux green: `#35c98f`
  - secondary blue accent: `#76a9ff`
  - warning amber for expiring signals: `#f2b84b`
- Use thin luminous lines for WSS connections.
- Use subtle glass/terminal panels, not glossy consumer SaaS cards.
- Use small readable labels only for core components.

## Diagram Language

Represent roles consistently:

- Hub: central routing node, server rack, or compact control-plane box.
- Worker: laptop/server box with tmux/PTY layer and local agent sessions.
- Control: browser/terminal client surface, often multi-pane.
- Cloudflare: edge/tunnel layer between public internet and Hub.
- SQLite: small durable local database cylinder near Hub.
- Join signal: short-lived amber token.
- Credential: scoped green key/token.

## Composition Rules

- Prefer 16:9 for documentation hero diagrams.
- Prefer 4:3 for embedded architecture diagrams.
- Avoid clutter. If more than 7 components are needed, split the concept into
  multiple images.
- Arrows should show direction:
  - Worker -> Hub: outbound WSS
  - Control -> Hub: HTTPS/WSS
  - Hub -> SQLite: local persistence
  - Worker -> tmux -> agent: local shell-layer control
- Text must be sparse and large enough to read in a 900px-wide Markdown page.

## Negative Style

Avoid:

- cartoon robots
- generic cloud-computing stock art
- overly purple/blue gradient SaaS backgrounds
- fake screenshots with illegible paragraphs
- crowded network maps
- random binary code rain
- 3D plastic icons
- photo-realistic people unless the prompt asks for a usage scenario
- terminal text that looks like real secrets or API keys
