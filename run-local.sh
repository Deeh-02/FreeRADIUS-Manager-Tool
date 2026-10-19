#!/bin/bash
# =============================================================================
# FreeRADIUS Manager - run locally WITHOUT Docker (Linux / macOS)
# Needs: PostgreSQL 14+ (running), Go 1.21+, Node 18+.
# Starts the API on :8080 and the web UI on http://localhost:3000.
# FreeRADIUS itself is not started; billing, plans, users and the UI work without it.
# =============================================================================
set -euo pipefail
cd "$(dirname "$0")"

info()  { echo "[INFO] $*"; }
fail()  { echo "[ERROR] $*" >&2; exit 1; }

for tool in psql go node npm openssl; do
  command -v "$tool" >/dev/null 2>&1 || fail "'$tool' is not installed or not on PATH"
done

# Local mode differs from Docker: API on :8080 (Docker: 8088), UI served by Vite on :3000
# (Docker: nginx on 8081), no FreeRADIUS and no nmap unless installed on the host. Pages
# that need tables missing from init-db.sql (see its header) will error here too.
ENV_FILE=".env.local"
if [ ! -f "$ENV_FILE" ]; then
  info "Creating $ENV_FILE with generated secrets"
  cat > "$ENV_FILE" <<EOF
DB_HOST=localhost
DB_PORT=${DB_PORT:-5432}
DB_USER=radius_local
DB_PASSWORD=$(openssl rand -hex 16)
DB_NAME=radius_local
JWT_SECRET=$(openssl rand -hex 32)
EOF
fi
set -a; . "./$ENV_FILE"; set +a

# Superuser access: try the current OS user first, then the 'postgres' account via sudo.
if psql -h "$DB_HOST" -p "$DB_PORT" -d postgres -qtAc "select 1" >/dev/null 2>&1; then
  admin_psql() { psql -h "$DB_HOST" -p "$DB_PORT" -v ON_ERROR_STOP=1 -q "$@"; }
elif command -v sudo >/dev/null 2>&1 && sudo -u postgres true; then
  admin_psql() { sudo -u postgres psql -p "$DB_PORT" -v ON_ERROR_STOP=1 -q "$@"; }
else
  fail "Cannot connect to PostgreSQL as an admin. Make sure it is running and that either your user or the 'postgres' user can log in without a password."
fi

info "Preparing database '$DB_NAME'"
admin_psql -d postgres -tAc "select 1 from pg_roles where rolname='$DB_USER'" | grep -q 1 \
  || admin_psql -d postgres -c "CREATE ROLE $DB_USER LOGIN PASSWORD '$DB_PASSWORD'"
admin_psql -d postgres -tAc "select 1 from pg_database where datname='$DB_NAME'" | grep -q 1 \
  || admin_psql -d postgres -c "CREATE DATABASE $DB_NAME OWNER $DB_USER"
# init-db.sql needs these extensions, which only a superuser can create.
admin_psql -d "$DB_NAME" -c 'CREATE EXTENSION IF NOT EXISTS "uuid-ossp"; CREATE EXTENSION IF NOT EXISTS pgcrypto;'

app_psql() { PGPASSWORD="$DB_PASSWORD" psql -h "$DB_HOST" -p "$DB_PORT" -U "$DB_USER" -d "$DB_NAME" -v ON_ERROR_STOP=1 -q "$@"; }

if ! app_psql -tAc "select to_regclass('public.app_users')" | grep -q app_users; then
  info "Creating base schema"
  app_psql -f init-db.sql
fi
# Migrations are idempotent, so they are re-applied on every run; NOTICE lines are hidden
# and `|| true` means a failing migration is not reported unless you read the output.
for f in migrations/*.sql; do
  info "Applying $f"
  app_psql -f "$f" 2>&1 | grep -v NOTICE || true
done

info "Building backend"
mkdir -p .local
(cd backend && go build -o ../.local/radius-manager .)

if [ ! -d frontend/node_modules ]; then
  info "Installing frontend dependencies"
  (cd frontend && npm install --no-audit --no-fund --silent)
fi

PIDS=()
cleanup() { info "Stopping"; for p in "${PIDS[@]}"; do kill "$p" 2>/dev/null || true; done; }
trap cleanup EXIT INT TERM

info "Starting API on http://localhost:8080"
DB_SSL_MODE=disable WEB_PORT=8080 APP_ENV=development \
  RADIUS_HOST=localhost RADIUS_SECRET=local-dev-secret ./.local/radius-manager > .local/backend.log 2>&1 &
PIDS+=($!)

for _ in $(seq 1 30); do
  curl -sf http://localhost:8080/health >/dev/null 2>&1 && break
  kill -0 "${PIDS[0]}" 2>/dev/null || { tail -20 .local/backend.log; fail "Backend exited. See .local/backend.log"; }
  sleep 1
done
curl -sf http://localhost:8080/health >/dev/null || fail "Backend did not become healthy. See .local/backend.log"

info "Starting web UI"
(cd frontend && exec node node_modules/vite/bin/vite.js --host 127.0.0.1) > .local/frontend.log 2>&1 &
PIDS+=($!)

echo ""
echo "  Web UI:  http://localhost:3000   (first visit: complete the setup wizard to create your admin)"
echo "  API:     http://localhost:8080"
echo "  Logs:    .local/backend.log  .local/frontend.log"
echo "  Press Ctrl+C to stop."
echo ""
wait
