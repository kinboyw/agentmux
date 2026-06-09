# 03. Cloudflare Deployment

## Purpose

Explain the recommended deployment topology:

```text
Browser/Worker/Control -> Cloudflare HTTPS/WSS -> cloudflared tunnel -> Go Hub -> SQLite
```

## Recommended Placement

- `docs/DEPLOYMENT.md`
- Cloudflare setup section in README
- Product architecture deployment section

## Aspect Ratio

16:9, 1920x1080.

## Positive Prompt

Technical deployment topology diagram for AgentMux running behind Cloudflare Tunnel. Use a dark neutral background and clean luminous connection lines. Left side: three clients labeled "Browser Control", "CLI Control", "Worker". They connect over "HTTPS / WSS" to an edge layer labeled "Cloudflare". From Cloudflare, show a secure tunnel labeled "cloudflared tunnel" going to a private server box labeled "AgentMux Hub :8080". Next to Hub, show a local disk/database cylinder labeled "SQLite agentmux.db". Include a small note visually: "no inbound worker ports" near the Worker path. The Hub server should look self-hosted, not serverless. Keep Cloudflare as a generic edge/tunnel layer without using trademarked logo if logo accuracy is uncertain. Clean diagram, readable labels, precise arrows, no visual noise.

## Negative Prompt

Official-looking Cloudflare logo unless exact, D1 as the main database, Kubernetes cluster, complex service mesh, dozens of nodes, generic cloud icons, people, mascot robots, unreadable labels, bright purple gradients.

## Review Checklist

- Cloudflare sits in front of Hub, not replacing Hub.
- SQLite is local to the Go Hub server.
- Worker path is outbound-only.
- WSS/HTTPS labels are visible.
- Does not imply current Hub runs inside Cloudflare Workers.
