#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SESSION="${AGENTMUX_DEV_SESSION:-agentmux-dev}"
PORT="${AGENTMUX_PORT:-8081}"
TOKEN="${AGENTMUX_TOKEN:-dev-token}"
WORKER="${AGENTMUX_WORKER:-local}"
DEMO_SESSION="${AGENTMUX_DEMO_SESSION:-demo}"
HUB_HTTP="http://127.0.0.1:${PORT}"
HUB_WS="ws://127.0.0.1:${PORT}"

quote() {
  printf '%q' "$1"
}

if ! command -v tmux >/dev/null 2>&1; then
  echo "tmux is required" >&2
  exit 1
fi

if ! command -v go >/dev/null 2>&1; then
  echo "go is required" >&2
  exit 1
fi

if tmux has-session -t "${SESSION}" 2>/dev/null; then
  tmux attach -t "${SESSION}"
  exit 0
fi

HUB_CMD="go run ./cmd/agentmux hub --addr $(quote "127.0.0.1:${PORT}") --token $(quote "${TOKEN}")"
WORKER_CMD="sleep 1; go run ./cmd/agentmux worker --hub $(quote "${HUB_WS}") --token $(quote "${TOKEN}") --name $(quote "${WORKER}")"
CONTROL_CMD="sleep 3; go run ./cmd/agentmux control workers --hub $(quote "${HUB_HTTP}") --token $(quote "${TOKEN}"); echo; go run ./cmd/agentmux control create --hub $(quote "${HUB_HTTP}") --token $(quote "${TOKEN}") --worker $(quote "${WORKER}") --name $(quote "${DEMO_SESSION}") --cwd $(quote "${ROOT_DIR}") --command bash || true; echo; echo $(quote "Attaching to ${WORKER}/${DEMO_SESSION}. Type commands and press Enter."); go run ./cmd/agentmux control attach --hub $(quote "${HUB_WS}") --token $(quote "${TOKEN}") --session $(quote "${WORKER}/${DEMO_SESSION}")"

tmux new-session -d -s "${SESSION}" -n dev -c "${ROOT_DIR}" "${HUB_CMD}"
tmux split-window -h -t "${SESSION}:dev" -c "${ROOT_DIR}" "${WORKER_CMD}"
tmux split-window -v -t "${SESSION}:dev.2" -c "${ROOT_DIR}" "${CONTROL_CMD}"
tmux select-pane -t "${SESSION}:dev.3"
tmux attach -t "${SESSION}"
