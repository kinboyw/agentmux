# 05. Agent-Unaware Shell Layer

## Purpose

Explain the central design choice: AgentMux attaches below the agent at the
shell/tmux/PTY layer, so the agent does not need built-in remote access.

## Recommended Placement

- `docs/DESIGN.md`
- Product architecture invariant section
- Landing page design philosophy

## Aspect Ratio

4:3, 1600x1200.

## Positive Prompt

Layered technical cutaway diagram for AgentMux. Show a vertical stack on a worker machine: top layer "Agent CLI" with examples "Codex / Claude / Shell"; middle layer "tmux session"; lower layer "PTY / shell"; sidecar layer "AgentMux Worker" observing and controlling tmux. The AgentMux Worker connects outward to "Hub" using WSS. Use arrows to show AgentMux does not inject into the agent; it controls terminal input/output at tmux/PTY level. Use dark technical editorial style, thin outlines, green and blue accents, concise labels. The composition should make "agent unaware" visually obvious with a small boundary line around the agent process.

## Negative Prompt

Robots, neural network brains, SDK plugin imagery, agent calling API callbacks, invasive code injection visuals, complex OS kernel diagram, unreadable labels, bright cyberpunk style.

## Review Checklist

- Agent CLI is above tmux/PTY and separate from AgentMux Worker.
- The remote path starts from Worker to Hub, not from Agent CLI.
- The phrase "agent unaware" can be inferred visually.
- The diagram is simple enough for design docs.
