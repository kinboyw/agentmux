# AgentMux Visual Prompt Library

This directory stores image-generation prompts for AgentMux documentation and
product pages. Treat these as source assets: prompts should evolve with product
architecture, just like API docs.

## Style Baseline

Use `style-baseline.md` as the shared visual direction for all generated images.
Each prompt file defines:

- purpose
- recommended placement
- aspect ratio
- positive prompt
- negative prompt
- post-generation review checklist

## Prompt Set

| File | Image Goal | Recommended Use |
| --- | --- | --- |
| `01-system-architecture.md` | Hub/worker/control architecture | README, architecture docs |
| `02-zero-tier-style-onboarding.md` | Signal-based onboarding flow | Landing page, quick start docs |
| `03-cloudflare-deployment.md` | Cloudflare Tunnel deployment topology | Deployment docs |
| `04-web-control-workspace.md` | Multi-pane browser control workspace | Web Control docs, landing page |
| `05-agent-unaware-shell-layer.md` | Agent-unaware tmux/shell bridge | Design docs |
| `06-token-and-tenant-model.md` | Signal/credential/tenant lifecycle | API/auth docs |
| `07-relay-vs-direct-mode.md` | Relay now, direct mode later | Roadmap docs |
| `08-operational-scenarios.md` | Multi-device remote usage scene | Product page, README hero |

## Naming Convention

Generated images should be stored outside this prompt directory, for example:

```text
docs/assets/visuals/agentmux-system-architecture-v1.png
docs/assets/visuals/agentmux-cloudflare-deployment-v1.png
```

Keep prompt revisions here and image binaries in `docs/assets/visuals/`.

## Global Constraints

- Do not include fake brand logos unless explicitly intended.
- Avoid tiny unreadable UI text.
- Prefer clean diagrams, product illustrations, and technical editorial visuals.
- Keep AgentMux concepts explicit: Hub, Worker, Control, tmux, WSS, SQLite,
  Cloudflare Tunnel, join signal, scoped credential.
- Use English labels in images unless a specific Chinese-localized image is
  requested later.

## Iteration Workflow

1. Pick one prompt file.
2. Generate 2-4 variants with the same prompt.
3. Review against the checklist in that file.
4. Save the best image under `docs/assets/visuals/`.
5. Update the prompt file with notes about what worked.
6. Reference the image from README or docs.

Suggested generated image metadata block:

```md
## Generation Notes

- Tool:
- Date:
- Model:
- Seed:
- Selected output:
- Follow-up changes:
```

## Recommended First Batch

Start with:

1. `01-system-architecture.md`
2. `03-cloudflare-deployment.md`
3. `04-web-control-workspace.md`
4. `02-zero-tier-style-onboarding.md`

These cover the README, deployment docs, landing page, and quick start.
