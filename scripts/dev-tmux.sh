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
DATA_PATH="${AGENTMUX_DATA:-${ROOT_DIR}/.tmp/agentmux-dev.db}"
ATTACH="${AGENTMUX_ATTACH:-1}"
RESET="${AGENTMUX_RESET:-0}"

quote() {
  printf '%q' "$1"
}

pane_cmd() {
  local role="$1"
  local cmd="$2"
  printf 'printf "\\033]2;%s\\033\\\\"; echo "[agentmux:%s] %s"; %s; code=$?; echo; echo "[agentmux:%s] exited with code ${code}"; echo "Press Enter to close this pane."; read -r _' \
    "${role}" "${role}" "${cmd}" "${cmd}" "${role}"
}

if ! command -v tmux >/dev/null 2>&1; then
  echo "tmux is required" >&2
  exit 1
fi

if command -v mise >/dev/null 2>&1; then
  GO_CMD="mise exec -- go"
elif command -v go >/dev/null 2>&1; then
  GO_CMD="go"
else
  echo "mise or go is required" >&2
  exit 1
fi

mkdir -p "$(dirname "${DATA_PATH}")"

if [ "${RESET}" = "1" ] && tmux has-session -t "${SESSION}" 2>/dev/null; then
  tmux kill-session -t "${SESSION}"
fi

if tmux has-session -t "${SESSION}" 2>/dev/null; then
  if [ "${ATTACH}" = "0" ]; then
    echo "tmux session already running: ${SESSION}"
    exit 0
  fi
  tmux attach -t "${SESSION}"
  exit 0
fi

HUB_CMD="${GO_CMD} run ./cmd/agentmux hub --addr $(quote "127.0.0.1:${PORT}") --token $(quote "${TOKEN}") --data $(quote "${DATA_PATH}") --public-url $(quote "${HUB_HTTP}")"
WORKER_CMD="sleep 1; ${GO_CMD} run ./cmd/agentmux worker --hub $(quote "${HUB_WS}") --token $(quote "${TOKEN}") --name $(quote "${WORKER}")"
CONTROL_CMD="sleep 3; ${GO_CMD} run ./cmd/agentmux control workers --hub $(quote "${HUB_HTTP}") --token $(quote "${TOKEN}"); echo; ${GO_CMD} run ./cmd/agentmux control create --hub $(quote "${HUB_HTTP}") --token $(quote "${TOKEN}") --worker $(quote "${WORKER}") --name $(quote "${DEMO_SESSION}") --cwd $(quote "${ROOT_DIR}") --command bash || true; echo; echo $(quote "Attaching to ${WORKER}/${DEMO_SESSION}. Type commands and press Enter."); ${GO_CMD} run ./cmd/agentmux control attach --hub $(quote "${HUB_WS}") --token $(quote "${TOKEN}") --session $(quote "${WORKER}/${DEMO_SESSION}")"

HUB_PANE="$(tmux new-session -d -P -F '#{pane_id}' -s "${SESSION}" -n dev -c "${ROOT_DIR}" "$(pane_cmd hub "${HUB_CMD}")")"
WORKER_PANE="$(tmux split-window -h -P -F '#{pane_id}' -t "${HUB_PANE}" -c "${ROOT_DIR}" "$(pane_cmd worker "${WORKER_CMD}")")"
CONTROL_PANE="$(tmux split-window -v -P -F '#{pane_id}' -t "${WORKER_PANE}" -c "${ROOT_DIR}" "$(pane_cmd control "${CONTROL_CMD}")")"
tmux select-pane -t "${CONTROL_PANE}"

if [ "${ATTACH}" = "0" ]; then
  echo "tmux session started: ${SESSION}"
  exit 0
fi

tmux attach -t "${SESSION}"
