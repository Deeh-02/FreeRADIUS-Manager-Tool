#!/bin/bash
# =============================================================================
# FreeRADIUS Manager - first-time setup (Linux / macOS)
# Writes .env with generated secrets (from .env.example), builds and starts the stack.
# Re-running it keeps an existing .env.
# =============================================================================
set -euo pipefail
cd "$(dirname "$0")"

GREEN='\033[0;32m'; YELLOW='\033[1;33m'; RED='\033[0;31m'; BLUE='\033[0;34m'; NC='\033[0m'
info()    { echo -e "${BLUE}[INFO]${NC} $*"; }
success() { echo -e "${GREEN}[OK]${NC} $*"; }
warn()    { echo -e "${YELLOW}[WARN]${NC} $*"; }
error()   { echo -e "${RED}[ERROR]${NC} $*" >&2; exit 1; }

echo -e "\n${BLUE}=== FreeRADIUS Manager - setup ===${NC}\n"

# --- Prerequisites ---
command -v docker >/dev/null 2>&1 || error "Docker is not installed: https://docs.docker.com/get-docker/"
if docker compose version >/dev/null 2>&1; then COMPOSE="docker compose"
elif command -v docker-compose >/dev/null 2>&1; then COMPOSE="docker-compose"
else error "Docker Compose is not installed."; fi
docker info >/dev/null 2>&1 || error "The Docker daemon is not running (or you need sudo / the docker group)."
command -v openssl >/dev/null 2>&1 || error "openssl is needed to generate secrets."
success "Docker, Compose and openssl found"

# Letters and digits only: some routers mishandle symbols in RADIUS secrets.
rand_alnum() { openssl rand -base64 64 | tr -dc 'A-Za-z0-9' | head -c "$1"; }

# --- .env ---
if [ -f .env ]; then
  warn ".env already exists; keeping it. Delete it and re-run to generate new secrets"
  warn "(the database password is fixed when the database is first created)."
else
  [ -f .env.example ] || error ".env.example is missing."
  tz="UTC"
  if command -v timedatectl >/dev/null 2>&1; then
    tz=$(timedatectl show -p Timezone --value 2>/dev/null || echo UTC)
  elif [ -f /etc/timezone ]; then
    tz=$(cat /etc/timezone)
  fi
  db_pass=$(rand_alnum 40); jwt=$(openssl rand -hex 32)
  radius=$(rand_alnum 32); token=$(rand_alnum 24)
  sed -e "s|^DB_PASSWORD=.*|DB_PASSWORD=${db_pass}|" \
      -e "s|^JWT_SECRET=.*|JWT_SECRET=${jwt}|" \
      -e "s|^RADIUS_SECRET=.*|RADIUS_SECRET=${radius}|" \
      -e "s|^SETUP_TOKEN=.*|SETUP_TOKEN=${token}|" \
      -e "s|^TZ=.*|TZ=${tz:-UTC}|" \
      .env.example > .env
  chmod 600 .env
  success ".env created with generated secrets (timezone: ${tz:-UTC}). Edit DEFAULT_CURRENCY and SMTP there."
fi

envval() { grep -E "^$1=" .env | tail -1 | cut -d= -f2- | tr -d '\r' || true; }
mkdir -p backups

# --- Build and start ---
info "Building Docker images (3-10 minutes the first time) ..."
$COMPOSE build
info "Starting services ..."
$COMPOSE up -d

wait_healthy() {
  local name=$1 max=$2 waited=0 status
  echo -n "  Waiting for $name"
  while [ "$waited" -lt "$max" ]; do
    status=$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$name" 2>/dev/null || echo missing)
    if [ "$status" = "healthy" ]; then echo ""; return 0; fi
    echo -n "."; sleep 3; waited=$((waited + 3))
  done
  echo ""; return 1
}

for c in radius_postgres radius_backend radius_freeradius radius_frontend; do
  if wait_healthy "$c" 120; then success "$c is healthy"
  else warn "$c is not healthy. Check: docker logs $c"; fi
done

ui_port=$(envval UI_PORT); ui_port=${ui_port:-8081}
ip=$(hostname -I 2>/dev/null | awk '{print $1}'); ip=${ip:-localhost}
echo ""
echo -e "${GREEN}=== Setup complete ===${NC}"
echo ""
echo -e "  Web UI:        http://${ip}:${ui_port}"
echo -e "  RADIUS:        ${ip} UDP 1812 (auth) / 1813 (accounting)"
echo -e "  Setup token:   $(envval SETUP_TOKEN)   (the setup wizard asks for it; also in .env)"
echo ""
echo -e "  ${YELLOW}Next:${NC}"
echo "  1. Open the web UI and complete the setup wizard to create the admin account."
echo "  2. Add each router on the NAS Devices page with its own secret (docs: DEVICE-INTEGRATION.md)."
echo "  3. Firewall: allow UDP 1812/1813 only from your routers, and the UI port only from staff."
echo "  4. Copy ./backups off this server regularly."
echo ""
echo "  Logs: $COMPOSE logs -f     Stop: $COMPOSE down     Update: ./deploy-update.sh"
echo ""
