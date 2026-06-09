# 06. Token And Tenant Model

## Purpose

Visualize anonymous signals, scoped credentials, and tenant isolation.

## Recommended Placement

- `docs/API.md`
- `docs/PRODUCT_ARCHITECTURE.md`
- Future auth docs

## Aspect Ratio

16:9, 1920x1080.

## Positive Prompt

Precise security lifecycle diagram for AgentMux auth. Dark background, clean line art. Left: "amx_sig_..." amber token with clock icon labeled "short-lived join signal". Arrow to central Hub box labeled "POST /api/exchange". From Hub, two green scoped credential tokens branch out: "worker credential" and "control credential". Both tokens are inside a larger boundary labeled "tenant_id". Show multiple tenant boundaries as separate subtle boxes to communicate isolation. Include tiny labels for scopes: "worker:connect", "session:list", "session:attach". Use lock/key iconography sparingly. Make it feel like infrastructure security documentation, not consumer login marketing.

## Negative Prompt

Padlocks everywhere, blockchain wallet imagery, real secrets, OAuth provider logos, cluttered permission matrix, unreadable small scope lists, cartoon hackers, red danger theme.

## Review Checklist

- Signal and credential are visually distinct.
- Exchange step is central.
- Tenant isolation is clear.
- Worker/control credentials have different roles.
- No raw signal appears as a normal API credential.
