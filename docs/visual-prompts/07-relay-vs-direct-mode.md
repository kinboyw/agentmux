# 07. Relay Vs Direct Mode

## Purpose

Show the current reliable relay mode and future direct-mode optimization.

## Recommended Placement

- `docs/ROADMAP.md`
- Product architecture routing modes
- Technical roadmap discussions

## Aspect Ratio

16:9, 1920x1080.

## Positive Prompt

Split-screen technical comparison diagram for AgentMux routing modes. Left panel titled "Relay Mode Now": Worker connects outbound to Hub, Control connects to Hub, terminal bytes route through Hub. Show this as a simple triangle with Hub at top center, Worker and Control at bottom corners, green WSS lines passing through Hub. Right panel titled "Direct Mode Later": Hub remains as signaling/auth broker, but Worker and Control have a direct data channel between them with Hub coordinating candidates. Use dotted line from Hub to both endpoints labeled "signaling + auth", and solid line between Worker and Control labeled "direct stream". Dark technical background, restrained green and blue accents, minimal readable labels. Clearly mark relay as reliable default and direct as optional future.

## Negative Prompt

P2P crypto network imagery, overly complex NAT traversal diagrams, dozens of arrows, VPN brand logos, confusing mesh, photorealistic cables, unreadable labels, sensational speed effects.

## Review Checklist

- Relay mode path clearly goes through Hub.
- Direct mode still uses Hub for auth/signaling.
- Direct mode is visually future/optional, not current default.
- Worker and Control roles remain consistent.
