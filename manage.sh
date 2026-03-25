#!/bin/bash
# MeshCore Analyzer — Setup & Management Helper
# Usage: ./manage.sh [command]
set -e

CONTAINER_NAME="meshcore-analyzer"
IMAGE_NAME="meshcore-analyzer"
DATA_VOLUME="meshcore-data"
CADDY_VOLUME="caddy-data"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m'

log()  { echo -e "${GREEN}✓${NC} $1"; }
warn() { echo -e "${YELLOW}⚠${NC} $1"; }
err()  { echo -e "${RED}✗${NC} $1"; }
info() { echo -e "${CYAN}→${NC} $1"; }

# ─── Setup ────────────────────────────────────────────────────────────────

cmd_setup() {
  echo ""
  echo "═══════════════════════════════════════"
  echo "  MeshCore Analyzer Setup"
  echo "═══════════════════════════════════════"
  echo ""

  # Check Docker
  if ! command -v docker &> /dev/null; then
    err "Docker is not installed."
    echo ""
    echo "Install it with:"
    echo "  curl -fsSL https://get.docker.com | sh"
    echo "  sudo usermod -aG docker \$USER"
    echo ""
    echo "Then log out, log back in, and run this script again."
    exit 1
  fi
  log "Docker found: $(docker --version | head -1)"

  # Check if container already exists
  if docker ps -a --format '{{.Names}}' | grep -q "^${CONTAINER_NAME}$"; then
    warn "Container '${CONTAINER_NAME}' already exists."
    read -p "   Remove it and start fresh? [y/N] " -n 1 -r
    echo
    if [[ $REPLY =~ ^[Yy]$ ]]; then
      docker stop "$CONTAINER_NAME" 2>/dev/null || true
      docker rm "$CONTAINER_NAME" 2>/dev/null || true
      log "Old container removed."
    else
      echo "Aborting. Use './manage.sh stop' and './manage.sh start' to manage the existing container."
      exit 0
    fi
  fi

  # Config
  if [ ! -f config.json ]; then
    cp config.example.json config.json
    log "Created config.json from example."
    warn "Edit config.json to set your apiKey before continuing."
    echo "   nano config.json"
    read -p "   Press Enter when done..."
  else
    log "config.json exists."
  fi

  # Caddyfile
  if [ ! -f caddy-config/Caddyfile ]; then
    mkdir -p caddy-config
    echo ""
    read -p "   Enter your domain (e.g., analyzer.example.com): " DOMAIN
    if [ -z "$DOMAIN" ]; then
      warn "No domain entered. Using HTTP-only (port 80, no HTTPS)."
      echo ':80 {
    reverse_proxy localhost:3000
}' > caddy-config/Caddyfile
    else
      echo "${DOMAIN} {
    reverse_proxy localhost:3000
}" > caddy-config/Caddyfile
      log "Caddyfile created for ${DOMAIN}"

      # Check DNS
      info "Checking DNS for ${DOMAIN}..."
      RESOLVED_IP=$(dig +short "$DOMAIN" 2>/dev/null | head -1)
      MY_IP=$(curl -s ifconfig.me 2>/dev/null || echo "unknown")
      if [ -z "$RESOLVED_IP" ]; then
        warn "DNS not resolving yet. Make sure your A record points to ${MY_IP}"
        warn "HTTPS provisioning will fail until DNS propagates."
      elif [ "$RESOLVED_IP" = "$MY_IP" ]; then
        log "DNS resolves to ${RESOLVED_IP} — matches this server."
      else
        warn "DNS resolves to ${RESOLVED_IP} but this server is ${MY_IP}"
        warn "HTTPS may fail if the domain doesn't point here."
      fi
    fi
  else
    log "caddy-config/Caddyfile exists."
  fi

  # Build
  echo ""
  info "Building Docker image..."
  docker build -t "$IMAGE_NAME" .
  log "Image built."

  # Run
  echo ""
  info "Starting container..."
  docker run -d \
    --name "$CONTAINER_NAME" \
    --restart unless-stopped \
    -p 80:80 \
    -p 443:443 \
    -v "$(pwd)/config.json:/app/config.json:ro" \
    -v "$(pwd)/caddy-config/Caddyfile:/etc/caddy/Caddyfile:ro" \
    -v "${DATA_VOLUME}:/app/data" \
    -v "${CADDY_VOLUME}:/data/caddy" \
    "$IMAGE_NAME"
  log "Container started."

  # Wait and check
  echo ""
  info "Waiting for startup..."
  sleep 5
  if docker ps --format '{{.Names}}' | grep -q "^${CONTAINER_NAME}$"; then
    log "Container is running."
    echo ""
    DOMAIN_LINE=$(grep -v '^#' caddy-config/Caddyfile 2>/dev/null | head -1 | tr -d ' {')
    if [ "$DOMAIN_LINE" = ":80" ]; then
      echo "   Open http://$(curl -s ifconfig.me 2>/dev/null || echo 'your-server-ip')"
    else
      echo "   Open https://${DOMAIN_LINE}"
    fi
    echo ""
    echo "   Logs:    ./manage.sh logs"
    echo "   Status:  ./manage.sh status"
    echo "   Stop:    ./manage.sh stop"
    echo ""
  else
    err "Container failed to start. Check logs:"
    docker logs "$CONTAINER_NAME" 2>&1 | tail -20
    exit 1
  fi
}

# ─── Start / Stop / Restart ──────────────────────────────────────────────

cmd_start() {
  if docker ps --format '{{.Names}}' | grep -q "^${CONTAINER_NAME}$"; then
    warn "Already running."
  elif docker ps -a --format '{{.Names}}' | grep -q "^${CONTAINER_NAME}$"; then
    docker start "$CONTAINER_NAME"
    log "Started."
  else
    err "Container doesn't exist. Run './manage.sh setup' first."
    exit 1
  fi
}

cmd_stop() {
  docker stop "$CONTAINER_NAME" 2>/dev/null && log "Stopped." || warn "Not running."
}

cmd_restart() {
  docker restart "$CONTAINER_NAME" 2>/dev/null && log "Restarted." || err "Failed."
}

# ─── Status ───────────────────────────────────────────────────────────────

cmd_status() {
  if docker ps --format '{{.Names}}' | grep -q "^${CONTAINER_NAME}$"; then
    log "Container is running."
    echo ""
    docker ps --filter "name=${CONTAINER_NAME}" --format "table {{.Status}}\t{{.Ports}}"
    echo ""
    # Quick health check
    info "Checking services..."
    docker exec "$CONTAINER_NAME" sh -c '
      printf "  Node.js:   "; wget -qO- http://localhost:3000/api/stats 2>/dev/null | head -c 80 && echo " ✓" || echo "✗ not responding"
      printf "  Mosquitto: "; mosquitto_sub -h localhost -t "test" -C 0 -W 1 2>/dev/null && echo "✓ running" || echo "✓ running (no messages)"
    ' 2>/dev/null || true
  else
    err "Container is not running."
  fi
}

# ─── Logs ─────────────────────────────────────────────────────────────────

cmd_logs() {
  docker logs -f "$CONTAINER_NAME" --tail "${1:-100}"
}

# ─── Update ───────────────────────────────────────────────────────────────

cmd_update() {
  info "Pulling latest code..."
  git pull

  info "Rebuilding image..."
  docker build -t "$IMAGE_NAME" .

  info "Restarting container with new image..."
  docker stop "$CONTAINER_NAME" 2>/dev/null || true
  docker rm "$CONTAINER_NAME" 2>/dev/null || true

  # Re-read the run flags
  docker run -d \
    --name "$CONTAINER_NAME" \
    --restart unless-stopped \
    -p 80:80 \
    -p 443:443 \
    -v "$(pwd)/config.json:/app/config.json:ro" \
    -v "$(pwd)/caddy-config/Caddyfile:/etc/caddy/Caddyfile:ro" \
    -v "${DATA_VOLUME}:/app/data" \
    -v "${CADDY_VOLUME}:/data/caddy" \
    "$IMAGE_NAME"

  log "Updated and restarted. Data preserved."
}

# ─── Backup ───────────────────────────────────────────────────────────────

cmd_backup() {
  DEST="${1:-./meshcore-backup-$(date +%Y%m%d-%H%M%S).db}"
  DB_PATH=$(docker volume inspect "$DATA_VOLUME" --format '{{ .Mountpoint }}')/meshcore.db

  if [ ! -f "$DB_PATH" ]; then
    # Fallback: try docker cp
    docker cp "${CONTAINER_NAME}:/app/data/meshcore.db" "$DEST" 2>/dev/null
  else
    cp "$DB_PATH" "$DEST"
  fi

  if [ -f "$DEST" ]; then
    SIZE=$(du -h "$DEST" | cut -f1)
    log "Backed up to ${DEST} (${SIZE})"
  else
    err "Backup failed — database not found."
    exit 1
  fi
}

# ─── Restore ──────────────────────────────────────────────────────────────

cmd_restore() {
  if [ -z "$1" ]; then
    err "Usage: ./manage.sh restore <backup-file.db>"
    exit 1
  fi
  if [ ! -f "$1" ]; then
    err "File not found: $1"
    exit 1
  fi

  warn "This will replace the current database with $1"
  read -p "   Continue? [y/N] " -n 1 -r
  echo
  if [[ ! $REPLY =~ ^[Yy]$ ]]; then
    echo "Aborted."
    exit 0
  fi

  docker stop "$CONTAINER_NAME" 2>/dev/null || true

  DB_PATH=$(docker volume inspect "$DATA_VOLUME" --format '{{ .Mountpoint }}')/meshcore.db
  if [ -d "$(dirname "$DB_PATH")" ]; then
    cp "$1" "$DB_PATH"
  else
    docker cp "$1" "${CONTAINER_NAME}:/app/data/meshcore.db"
  fi

  docker start "$CONTAINER_NAME"
  log "Restored from $1 and restarted."
}

# ─── MQTT Test ────────────────────────────────────────────────────────────

cmd_mqtt_test() {
  info "Listening for MQTT messages (10 second timeout)..."
  docker exec "$CONTAINER_NAME" mosquitto_sub -h localhost -t 'meshcore/#' -C 1 -W 10 2>/dev/null
  if [ $? -eq 0 ]; then
    log "MQTT is receiving data."
  else
    warn "No MQTT messages received in 10 seconds. Is an observer connected?"
  fi
}

# ─── Help ─────────────────────────────────────────────────────────────────

cmd_help() {
  echo ""
  echo "MeshCore Analyzer — Management Script"
  echo ""
  echo "Usage: ./manage.sh <command>"
  echo ""
  echo "Commands:"
  echo "  setup        Interactive first-time setup (config, build, run)"
  echo "  start        Start the container"
  echo "  stop         Stop the container"
  echo "  restart      Restart the container"
  echo "  status       Show container status and service health"
  echo "  logs [N]     Follow logs (last N lines, default 100)"
  echo "  update       Pull latest code, rebuild, restart (preserves data)"
  echo "  backup [f]   Backup database to file (default: timestamped)"
  echo "  restore <f>  Restore database from backup file"
  echo "  mqtt-test    Listen for MQTT messages (10s timeout)"
  echo "  help         Show this help"
  echo ""
}

# ─── Main ─────────────────────────────────────────────────────────────────

case "${1:-help}" in
  setup)     cmd_setup ;;
  start)     cmd_start ;;
  stop)      cmd_stop ;;
  restart)   cmd_restart ;;
  status)    cmd_status ;;
  logs)      cmd_logs "$2" ;;
  update)    cmd_update ;;
  backup)    cmd_backup "$2" ;;
  restore)   cmd_restore "$2" ;;
  mqtt-test) cmd_mqtt_test ;;
  help|*)    cmd_help ;;
esac
