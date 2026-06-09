# 02. ZeroTier-Style Onboarding

## Purpose

Illustrate the product flow:

1. Open Hub landing page.
2. Generate a short-lived join signal.
3. Paste one worker command.
4. Open Web Control from another device.

## Recommended Placement

- Landing page quick start
- `README.md` signal onboarding
- `docs/API.md` signal exchange section

## Aspect Ratio

16:9, 1920x1080.

## Positive Prompt

Clean product-flow illustration for "AgentMux" onboarding, inspired by ZeroTier-style join flow but without using ZeroTier branding. Dark technical UI background. Four numbered steps across the image, connected by a thin green line: 1 "Hub landing page" shown as a minimal browser panel with a button "Generate signal"; 2 "amx_sig_..." shown as a glowing amber short-lived token with a small countdown ring; 3 "Worker command" shown as a terminal snippet running `agentmux worker --join ...`; 4 "Web Control" shown as a browser window with multi-pane terminals. Include a small transformation from amber signal to green scoped credential labeled "exchange". Keep text minimal, large, readable. Use AgentMux green accents, subtle blue for browser surfaces, amber for expiring signal. Technical, elegant, product documentation style.

## Negative Prompt

Marketing hero fluff, huge unreadable terminal blocks, real-looking secrets, cartoon characters, brand logos, exaggerated cyberpunk effects, crowded timeline, photorealistic office scene.

## Review Checklist

- Four-step onboarding is obvious.
- Signal is visually temporary and separate from credential.
- Worker command and Web Control are both represented.
- No implication that agent itself has remote capability.
