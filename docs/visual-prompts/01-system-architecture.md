# 01. System Architecture

## Purpose

Show the core AgentMux invariant:

> Workers own local tmux/PTY state. Hub coordinates identity, discovery, routing,
> and policy. Control renders and operates sessions.

## Recommended Placement

- `README.md`
- `docs/PRODUCT_ARCHITECTURE.md`
- Landing page architecture section

## Aspect Ratio

16:9, 1920x1080.

## Positive Prompt

Modern technical architecture diagram for a developer infrastructure product named "AgentMux". Dark graphite background, restrained green and blue accents. Three main labeled components arranged left to right: "Control" on the left as a browser window and terminal UI, "Hub" in the center as a compact routing/control-plane server, and "Worker" on the right as a laptop/server running "tmux / PTY" with several local agent sessions labeled "Codex", "Claude", "Shell". Use thin glowing connection lines: Control to Hub labeled "HTTPS / WSS", Worker to Hub labeled "outbound WSS". Near Hub, show a small local database cylinder labeled "SQLite" and a tiny key/token module labeled "Signals + Credentials". Make the key idea visually clear: agents are inside tmux on the worker and do not know about remote access. Use sparse readable labels only. High-quality clean vector-like editorial illustration, precise, calm, no clutter, no fake code paragraphs.

## Negative Prompt

Cartoon robots, cute mascots, generic cloud stock illustration, excessive gradients, purple SaaS blobs, unreadable tiny text, crowded microservices mesh, random logos, real API keys, photorealistic people, 3D plastic icons, noisy background.

## Review Checklist

- Control, Hub, Worker are clearly separated.
- Worker visibly owns tmux/PTY and local agent sessions.
- Connections show WSS/HTTPS directions.
- SQLite appears local to Hub, not as a managed cloud database.
- Text remains readable at Markdown width.
