# 07. Relay Vs Direct Mode

## Purpose

Show the preferred direct mode and automatic relay fallback.

## Recommended Placement

- `docs/ROADMAP.md`
- Product architecture routing modes
- Technical roadmap discussions

## Aspect Ratio

16:9, 1920x1080.

## Positive Prompt

Split-screen technical comparison diagram for AgentMux routing modes. Left panel titled "Direct Mode Preferred": Hub remains as signaling/auth broker, Worker and Control have a direct encrypted data channel between them, and Hub coordinates grants, candidates, and revocation. Use dotted lines from Hub to both endpoints labeled "signaling + auth + revoke", and a solid line between Worker and Control labeled "encrypted direct stream". Right panel titled "Relay Fallback": Worker connects outbound to Hub, Control connects to Hub, terminal bytes route through Hub only after direct negotiation fails. Show this as a simple triangle with Hub at top center, Worker and Control at bottom corners, muted WSS lines passing through Hub. Dark technical background, restrained green and blue accents, minimal readable labels. Clearly mark direct as preferred and relay as automatic fallback.

## Negative Prompt

P2P crypto network imagery, overly complex NAT traversal diagrams, dozens of arrows, VPN brand logos, confusing mesh, photorealistic cables, unreadable labels, sensational speed effects.

## Review Checklist

- Direct mode is visually primary and preferred.
- Relay mode path clearly goes through Hub as fallback.
- Direct mode still uses Hub for auth, signaling, and revocation.
- Worker and Control roles remain consistent.
