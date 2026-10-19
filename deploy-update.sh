#!/bin/bash
# =============================================================================
# FreeRADIUS Manager - production update
#
#   ./deploy-update.sh            back up, pull, build, show who loses access, ask, migrate,
#                                 restart, re-sync access
#   ./deploy-update.sh --yes      same, without the confirmation question (for automation)
#   ./deploy-update.sh --no-pull  deploy the code already checked out (no git fetch)
#
# Run it on the server, in the deployment checkout. It pulls DEPLOY_BRANCH (default main)
# with a fast-forward only, and stops if the checkout has local changes. Nothing is stopped
# or changed until the new images have built and you have confirmed.
# =============================================================================
set -euo pipefail

GREEN='\033[0;32m'; YELLOW='\033[1;33m'; RED='\033[0;31m'; BLUE='\033[0;34m'; NC='\033[0m'
info()    { echo -e "${BLUE}[INFO]${NC} $*"; }
success() { echo -e "${GREEN}[OK]${NC} $*"; }
warn()    { echo -e "${YELLOW}[WARN]${NC} $*"; }
error()   { echo -e "${RED}[ERROR]${NC} $*" >&2; exit 1; }

# Everything runs inside main, so bash has read the whole script before `git pull` can
# replace this file.
main() {
  local assume_yes=false pull=true
  for arg in "$@"; do
    case "$arg" in
      --yes|-y) assume_yes=true ;;
      --no-pull) pull=false ;;
      *) error "unknown option: $arg" ;;
    esac
  done

  cd "$(dirname "$0")"
  command -v docker >/dev/null 2>&1 || error "Docker is not installed"
  if docker compose version >/dev/null 2>&1; then COMPOSE="docker compose"
  elif command -v docker-compose >/dev/null 2>&1; then COMPOSE="docker-compose"
  else error "Docker Compose is not installed"; fi

  [ -f .env ] || error ".env not found. Run ./setup.sh first."
  # Read single values from .env without sourcing it (values may contain spaces or *).
  envval() { grep -E "^$1=" .env | tail -1 | cut -d= -f2- | tr -d '\r' || true; }
  for v in DB_PASSWORD JWT_SECRET RADIUS_SECRET; do
    [ -n "$(envval $v)" ] || error "$v is empty in .env. See .env.example."
  done
  local db_user db_name
  db_user="$(envval DB_USER)"; db_user="${db_user:-radius_user}"
  db_name="$(envval DB_NAME)"; db_name="${db_name:-radius}"

  info "Updating FreeRADIUS Manager in $(pwd)"

  # ── 1. Backup ────────────────────────────────────────────────────────────
  mkdir -p backups
  if docker ps --format '{{.Names}}' | grep -q '^radius_postgres$'; then
    local backup="backups/radius_pre_update_$(date +%Y%m%d_%H%M%S).sql.gz"
    info "Backing up the database to $backup ..."
    docker exec radius_postgres pg_dump -U "$db_user" --no-owner "$db_name" | gzip > "$backup" \
      || { rm -f "$backup"; error "Backup failed; nothing was changed."; }
    success "Backup saved ($(du -h "$backup" | cut -f1)). Copy it off this server if it matters."
  else
    warn "Postgres container is not running; skipping the backup."
  fi

  # ── 2. Code ──────────────────────────────────────────────────────────────
  if $pull; then
    local branch="${DEPLOY_BRANCH:-main}"
    git diff --quiet && git diff --cached --quiet \
      || error "This checkout has uncommitted changes; commit or stash them first (or use --no-pull)."
    info "Fetching origin/$branch ..."
    git fetch origin "$branch"
    if [ "$(git rev-parse --abbrev-ref HEAD)" != "$branch" ]; then
      info "Switching to $branch"
      git checkout "$branch"
    fi
    local before; before=$(git rev-parse HEAD)
    git merge --ff-only "origin/$branch" || error "Local $branch has diverged from origin/$branch; resolve that by hand."
    if [ "$before" = "$(git rev-parse HEAD)" ]; then
      info "Already up to date: $(git log -1 --oneline)"
    else
      success "Updated to: $(git log -1 --oneline)"
    fi
  else
    info "Using the current checkout: $(git log -1 --oneline 2>/dev/null || echo 'not a git checkout')"
  fi

  # ── 3. Build (the running stack is untouched if this fails) ──────────────
  info "Building images (this may take a few minutes) ..."
  $COMPOSE build backend frontend freeradius
  success "Images built"

  # ── 4. Who will be refused once access is enforced ───────────────────────
  $COMPOSE up -d postgres >/dev/null
  wait_healthy radius_postgres 60 || error "Postgres did not become healthy."
  info "Checking which customers will be refused after the update ..."
  local report
  report=$($COMPOSE run --rm --no-deps -T backend ./radius-manager access-report) \
    || error "access-report failed: $report"
  echo "$report"
  if ! echo "$report" | grep -q 'Refused after sync: 0 '; then
    if ! $assume_yes; then
      if [ -t 0 ]; then
        read -r -p "These customers will be refused by FreeRADIUS. Continue? [y/N] " answer
        [[ "$answer" =~ ^[Yy] ]] || error "Stopped. Nothing was changed; the old version is still running."
      else
        error "Not a terminal: re-run with --yes to accept the list above."
      fi
    fi
  fi

  # ── 5. Migrations (each file once, each in one transaction) ──────────────
  info "Applying database migrations ..."
  psql_db() { docker exec -i radius_postgres psql -U "$db_user" -d "$db_name" -v ON_ERROR_STOP=1 -q "$@"; }
  psql_db -c "SET client_min_messages = warning; CREATE TABLE IF NOT EXISTS schema_migrations (filename VARCHAR(200) PRIMARY KEY, applied_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP)"
  local f name out applied=0
  for f in $(ls migrations/*.sql 2>/dev/null | sort); do
    name=$(basename "$f")
    if [ "$(psql_db -tAc "SELECT 1 FROM schema_migrations WHERE filename = '$name'")" = "1" ]; then
      continue
    fi
    info "  → $name"
    if ! out=$(psql_db -1 -f - < "$f" 2>&1); then
      echo "$out" | grep -v 'NOTICE' >&2 || true
      error "Migration $name failed and was rolled back; the old version is still running."
    fi
    echo "$out" | grep -v 'NOTICE' || true
    psql_db -c "INSERT INTO schema_migrations (filename) VALUES ('$name')"
    applied=$((applied + 1))
  done
  success "Migrations: $applied applied"

  # ── 6. Restart ───────────────────────────────────────────────────────────
  info "Restarting containers ..."
  $COMPOSE up -d
  wait_healthy radius_backend 120 || error "The backend did not become healthy. Check: docker logs radius_backend"
  success "Backend is healthy"

  # ── 7. Re-sync RADIUS access for every customer ──────────────────────────
  info "Writing RADIUS access rows for every customer ..."
  docker exec radius_backend ./radius-manager resync-access || warn "Some customers failed to sync; see above."

  wait_healthy radius_freeradius 60 && success "FreeRADIUS is healthy" \
    || warn "FreeRADIUS is not healthy yet. Check: docker logs radius_freeradius"
  wait_healthy radius_frontend 60 && success "Web UI is healthy" \
    || warn "Web UI is not healthy yet. Check: docker logs radius_frontend"

  echo ""
  success "Update complete: $(curl -sf http://127.0.0.1:8088/api/v1/version 2>/dev/null || echo 'version unknown')"
  echo "  Web UI: http://$(hostname -I 2>/dev/null | awk '{print $1}'):$(envval UI_PORT | grep . || echo 8081)"
  warn "Hard-refresh your browser (Ctrl+Shift+R) to load the new UI."
}

# wait_healthy CONTAINER SECONDS: waits for Docker's healthcheck to report healthy.
wait_healthy() {
  local name=$1 max=$2 waited=0 status
  while [ "$waited" -lt "$max" ]; do
    status=$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$name" 2>/dev/null || echo missing)
    [ "$status" = "healthy" ] && return 0
    sleep 3; waited=$((waited + 3))
  done
  return 1
}

main "$@"
