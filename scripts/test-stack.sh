#!/usr/bin/env bash
# The whole system on this machine for end-to-end, failure and load tests: the scratch
# PostgreSQL (test-db.sh), a real FreeRADIUS (test-radius.sh) and the real backend binary,
# built from this checkout and started the way production starts it (APP_ENV=production,
# real secrets, background workers on).
#
#   scripts/test-stack.sh start          start all three; prints the variables the tests use
#   scripts/test-stack.sh stop           stop all three and delete their data
#   scripts/test-stack.sh kill-backend   kill -9 the backend (a crash, mid-request)
#   scripts/test-stack.sh start-backend  start the backend again
#
# Nothing here listens beyond 127.0.0.1.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DIR="${TEST_STACK_DIR:-/tmp/radius-manager-test-stack}"
API_PORT="${TEST_API_PORT:-18080}"
PG_PORT="${TEST_PG_PORT:-54329}"
RADIUS_PORT="${TEST_RADIUS_PORT:-18120}"
RADIUS_SECRET="${TEST_RADIUS_SECRET:-testing123-e2e}"

backend_pid() { cat "$DIR/backend.pid" 2>/dev/null || true; }

start_backend() {
  mkdir -p "$DIR"
  [ -f "$DIR/env" ] || {
    # Fresh secrets for each stack, as setup.sh would generate.
    cat >"$DIR/env" <<EOF
APP_ENV=production
WEB_PORT=$API_PORT
DB_HOST=127.0.0.1
DB_PORT=$PG_PORT
DB_USER=postgres
DB_PASSWORD=$(openssl rand -hex 16)
DB_NAME=radius_test
DB_MAX_OPEN_CONNS=${TEST_DB_MAX_OPEN_CONNS:-25}
JWT_SECRET=$(openssl rand -hex 32)
RADIUS_HOST=127.0.0.1
RADIUS_AUTH_PORT=$RADIUS_PORT
RADIUS_SECRET=$RADIUS_SECRET
RATE_LIMIT=${TEST_RATE_LIMIT:-100000}
BACKUP_DIR=$DIR/backups
TZ=${TZ:-Africa/Nairobi}
EOF
    # M-Pesa sandbox settings from your shell, if any (with a public tunnel to this API,
    # Safaricom can call it back; see docs/CLOUD-TEST.md).
    env | grep '^MPESA_' >>"$DIR/env" || true
  }
  [ -x "$DIR/radius-manager" ] || (cd "$ROOT/backend" && go build -o "$DIR/radius-manager" .)
  (cd "$DIR" && set -a && . ./env && set +a && exec ./radius-manager >>"$DIR/backend.log" 2>&1) &
  echo $! >"$DIR/backend.pid"
  for _ in $(seq 1 100); do
    curl -fs "http://127.0.0.1:$API_PORT/api/v1/setup/status" >/dev/null 2>&1 && return 0
    kill -0 "$(backend_pid)" 2>/dev/null || break
    sleep 0.2
  done
  echo "backend did not start; log:" >&2
  tail -30 "$DIR/backend.log" >&2
  exit 1
}

kill_backend() {
  local p; p="$(backend_pid)"
  if [ -n "$p" ]; then
    kill -9 "$p" 2>/dev/null || true
    while kill -0 "$p" 2>/dev/null; do sleep 0.05; done
  fi
  rm -f "$DIR/backend.pid"
}

case "${1:-}" in
  start)
    eval "$("$ROOT/scripts/test-db.sh" start)"
    eval "$("$ROOT/scripts/test-radius.sh" start)"
    kill_backend
    rm -f "$DIR/radius-manager"   # rebuild from this checkout
    start_backend
    echo "export TEST_DATABASE_URL=\"$TEST_DATABASE_URL\""
    echo "export TEST_RADIUS_ADDR=$TEST_RADIUS_ADDR TEST_RADIUS_SECRET=$TEST_RADIUS_SECRET"
    echo "export TEST_API_URL=http://127.0.0.1:$API_PORT TEST_STACK=$ROOT/scripts/test-stack.sh"
    ;;
  stop)
    kill_backend
    "$ROOT/scripts/test-radius.sh" stop
    "$ROOT/scripts/test-db.sh" stop
    rm -rf "$DIR"
    ;;
  kill-backend)  kill_backend ;;
  start-backend) start_backend ;;
  # Pass-throughs so a test needs only TEST_STACK.
  db-crash|db-down|db-up) "$ROOT/scripts/test-db.sh" "${1#db-}" ;;
  radius-down|radius-up|radius-restart|radius-pause|radius-resume) "$ROOT/scripts/test-radius.sh" "${1#radius-}" ;;
  log) tail -n "${2:-50}" "$DIR/backend.log" ;;
  *)
    echo "usage: $0 start|stop|kill-backend|start-backend|db-crash|db-down|db-up|radius-down|radius-up|radius-restart|radius-pause|radius-resume|log" >&2
    exit 2
    ;;
esac
