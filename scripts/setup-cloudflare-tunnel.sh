#!/usr/bin/env bash
set -euo pipefail

TUNNEL_NAME="${TUNNEL_NAME:-agentmux}"
HOSTNAME="${HOSTNAME:-cf-agentmux.kinboy.wang}"
ORIGIN_SERVICE="${ORIGIN_SERVICE:-http://127.0.0.1:8081}"
CONFIG_PATH="${CONFIG_PATH:-/etc/cloudflared/config.yml}"
CREDENTIALS_PATH="${CREDENTIALS_PATH:-/etc/cloudflared/${TUNNEL_NAME}.json}"
RUN_MODE="${RUN_MODE:-foreground}"
OVERWRITE_DNS="${OVERWRITE_DNS:-0}"

usage() {
  cat <<USAGE
Usage:
  TUNNEL_NAME=agentmux HOSTNAME=cf-agentmux.kinboy.wang ORIGIN_SERVICE=http://127.0.0.1:8081 \\
    RUN_MODE=foreground ./scripts/setup-cloudflare-tunnel.sh

Environment:
  TUNNEL_NAME       Cloudflare Tunnel name. Default: agentmux
  HOSTNAME          Public hostname routed to this tunnel. Default: cf-agentmux.kinboy.wang
  ORIGIN_SERVICE    Local origin service. Default: http://127.0.0.1:8081
  CONFIG_PATH       cloudflared config path. Default: /etc/cloudflared/config.yml
  CREDENTIALS_PATH  tunnel credentials path. Default: /etc/cloudflared/<tunnel>.json
  RUN_MODE          foreground | service | none. Default: foreground
  OVERWRITE_DNS     1 to pass --overwrite-dns when creating DNS route. Default: 0

The script installs cloudflared if missing, runs cloudflared tunnel login when
needed, creates or reuses a named tunnel, writes a local ingress config, creates
the DNS route, validates the config, and optionally starts cloudflared.
USAGE
}

log() {
  printf '[cloudflared-setup] %s\n' "$*"
}

die() {
  printf '[cloudflared-setup] error: %s\n' "$*" >&2
  exit 1
}

need() {
  command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"
}

sudo_cmd() {
  if [ "$(id -u)" -eq 0 ]; then
    "$@"
  else
    sudo "$@"
  fi
}

install_cloudflared() {
  if command -v cloudflared >/dev/null 2>&1; then
    log "cloudflared already installed: $(command -v cloudflared)"
    cloudflared --version || true
    return
  fi

  need curl
  need uname
  local os arch url tmp
  os="$(uname -s)"
  arch="$(uname -m)"

  case "$os" in
    Linux)
      case "$arch" in
        x86_64|amd64) url="https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-amd64" ;;
        aarch64|arm64) url="https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-arm64" ;;
        armv7l|armv6l) url="https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-arm" ;;
        *) die "unsupported Linux architecture: $arch" ;;
      esac
      tmp="$(mktemp)"
      log "downloading cloudflared from $url"
      curl -fsSL "$url" -o "$tmp"
      chmod +x "$tmp"
      sudo_cmd install -m 0755 "$tmp" /usr/local/bin/cloudflared
      rm -f "$tmp"
      ;;
    Darwin)
      if command -v brew >/dev/null 2>&1; then
        log "installing cloudflared with Homebrew"
        brew install cloudflared
      else
        die "macOS install requires Homebrew or a preinstalled cloudflared binary"
      fi
      ;;
    *)
      die "unsupported OS: $os"
      ;;
  esac

  cloudflared --version
}

ensure_login() {
  local cert="${HOME}/.cloudflared/cert.pem"
  if [ -f "$cert" ]; then
    log "cloudflared account certificate exists: $cert"
    return
  fi
  log "cloudflared is not logged in. A browser login will open."
  cloudflared tunnel login
}

tunnel_exists() {
  cloudflared tunnel info "$TUNNEL_NAME" >/dev/null 2>&1
}

ensure_tunnel() {
  if tunnel_exists; then
    log "tunnel exists: $TUNNEL_NAME"
  else
    log "creating tunnel: $TUNNEL_NAME"
    cloudflared tunnel create "$TUNNEL_NAME"
  fi
}

tunnel_id() {
  cloudflared tunnel list | awk -v name="$TUNNEL_NAME" '$2 == name {print $1; exit}'
}

install_credentials() {
  local id src
  id="$(tunnel_id)"
  [ -n "$id" ] || die "could not resolve tunnel id for $TUNNEL_NAME"

  if [ -f "$CREDENTIALS_PATH" ]; then
    log "credentials already installed: $CREDENTIALS_PATH"
    return
  fi

  src="${HOME}/.cloudflared/${id}.json"
  [ -f "$src" ] || die "tunnel credentials not found at $src"

  log "installing credentials to $CREDENTIALS_PATH"
  sudo_cmd install -d -m 0755 "$(dirname "$CREDENTIALS_PATH")"
  sudo_cmd install -m 0600 "$src" "$CREDENTIALS_PATH"
}

write_config() {
  log "writing config: $CONFIG_PATH"
  sudo_cmd install -d -m 0755 "$(dirname "$CONFIG_PATH")"
  if [ "$(id -u)" -eq 0 ]; then
    cat >"$CONFIG_PATH" <<EOF
tunnel: ${TUNNEL_NAME}
credentials-file: ${CREDENTIALS_PATH}

ingress:
  - hostname: ${HOSTNAME}
    service: ${ORIGIN_SERVICE}
  - service: http_status:404
EOF
  else
    sudo tee "$CONFIG_PATH" >/dev/null <<EOF
tunnel: ${TUNNEL_NAME}
credentials-file: ${CREDENTIALS_PATH}

ingress:
  - hostname: ${HOSTNAME}
    service: ${ORIGIN_SERVICE}
  - service: http_status:404
EOF
  fi
  sudo_cmd chmod 0644 "$CONFIG_PATH"
}

route_dns() {
  local args=()
  if [ "$OVERWRITE_DNS" = "1" ]; then
    args+=(--overwrite-dns)
  fi
  log "creating DNS route: $HOSTNAME -> $TUNNEL_NAME"
  cloudflared tunnel route dns "${args[@]}" "$TUNNEL_NAME" "$HOSTNAME" || {
    log "DNS route command failed. If the record already exists, set OVERWRITE_DNS=1 or verify it in Cloudflare."
  }
}

validate_config() {
  log "validating ingress config"
  cloudflared tunnel --config "$CONFIG_PATH" ingress validate
  cloudflared tunnel --config "$CONFIG_PATH" ingress rule "https://${HOSTNAME}" || true
}

start_tunnel() {
  case "$RUN_MODE" in
    foreground)
      log "starting tunnel in foreground"
      exec cloudflared tunnel --config "$CONFIG_PATH" run "$TUNNEL_NAME"
      ;;
    service)
      command -v systemctl >/dev/null 2>&1 || die "RUN_MODE=service requires systemctl"
      if systemctl list-unit-files cloudflared.service >/dev/null 2>&1; then
        log "cloudflared service exists; restarting"
        sudo_cmd systemctl restart cloudflared
      else
        log "installing cloudflared systemd service"
        sudo_cmd cloudflared --config "$CONFIG_PATH" service install
        sudo_cmd systemctl enable cloudflared
        sudo_cmd systemctl start cloudflared
      fi
      sudo_cmd systemctl status cloudflared --no-pager || true
      ;;
    none)
      log "RUN_MODE=none; not starting tunnel"
      ;;
    *)
      die "invalid RUN_MODE: $RUN_MODE"
      ;;
  esac
}

if [ "${1:-}" = "-h" ] || [ "${1:-}" = "--help" ]; then
  usage
  exit 0
fi

install_cloudflared
ensure_login
ensure_tunnel
install_credentials
write_config
route_dns
validate_config

log "AgentMux Hub should be started with:"
log "  agentmux hub --addr 127.0.0.1:8081 --data ./agentmux.db --public-url https://${HOSTNAME}"

start_tunnel
