#!/usr/bin/env bash
# Starts the real Docker images (docker compose, from this checkout) on an empty database and
# checks the things only the containers can show: every service becomes healthy, the first
# start applies and records every migration, the setup wizard and staff sign-in work through
# nginx, a customer created in the API is accepted by the real FreeRADIUS, and the M-Pesa
# callback secret stays out of the nginx and API logs. CI runs it on every push.
#
#   scripts/docker-smoke.sh          build, start, check, then remove everything (volumes too)
#   KEEP=1 scripts/docker-smoke.sh   leave the stack running afterwards to look around
#
# It writes a throwaway .env with random secrets when there is none, and refuses to run if
# the radius_* containers already exist (it would delete their volumes).
set -euo pipefail
cd "$(dirname "$0")/.."

ok()   { echo "  ok   $*"; }
fail() { echo "  FAIL $*" >&2; failed=1; }
failed=0
COMPOSE="docker compose"
UI="http://127.0.0.1:${UI_PORT:-8081}"

if docker ps -a --format '{{.Names}}' | grep -q '^radius_'; then
  echo "radius_* containers already exist; this test would delete their data. Stop here." >&2
  exit 2
fi

made_env=0
if [ ! -f .env ]; then
  made_env=1
  cat > .env <<EOF
DB_PASSWORD=$(openssl rand -hex 16)
JWT_SECRET=$(openssl rand -hex 32)
RADIUS_SECRET=$(openssl rand -hex 12)
SETUP_TOKEN=$(openssl rand -hex 12)
TZ=Africa/Nairobi
APP_ENV=production
BCRYPT_COST=10
EOF
fi
envval() { grep -E "^$1=" .env | tail -1 | cut -d= -f2-; }
RADIUS_SECRET=$(envval RADIUS_SECRET)
SETUP_TOKEN=$(envval SETUP_TOKEN)

cleanup() {
  if [ "$failed" = 1 ] || [ "${SHOW_LOGS:-}" = 1 ]; then
    $COMPOSE logs --no-color --tail 80 || true
  fi
  if [ -z "${KEEP:-}" ]; then
    $COMPOSE down -v --remove-orphans >/dev/null 2>&1 || true
    [ "$made_env" = 1 ] && rm -f .env
  fi
}
trap cleanup EXIT

echo "Building and starting the stack ..."
$COMPOSE up -d --build --wait --wait-timeout 420
ok "all services healthy"

json() { python3 -c "import json,sys; d=json.load(sys.stdin); print(d$1)"; }
psql_db() { docker exec -i radius_postgres psql -U "$(envval DB_USER | grep . || echo radius_user)" -d "$(envval DB_NAME | grep . || echo radius)" -tAq "$@"; }

echo "Checks:"
curl -fsS "$UI/nginx-health" >/dev/null && ok "nginx answers" || fail "nginx-health"
curl -fsS "$UI/api/v1/health" >/dev/null && ok "API answers through nginx" || fail "API health through nginx"

files=$(ls migrations/*.sql | wc -l)
recorded=$(psql_db -c "SELECT COUNT(*) FROM schema_migrations")
[ "$recorded" = "$files" ] && ok "first start recorded all $files migrations" \
  || fail "schema_migrations has $recorded rows, migrations/ has $files files"
[ "$(psql_db -c "SHOW timezone")" = "Africa/Nairobi" ] && ok "database runs in Africa/Nairobi" \
  || fail "database timezone is $(psql_db -c 'SHOW timezone')"

ADMIN_PW="Smoke-test-$(openssl rand -hex 6)!"
code=$(curl -s -o /tmp/smoke-setup.json -w '%{http_code}' -H 'Content-Type: application/json' -X POST "$UI/api/v1/setup/complete" \
  -d "{\"setup_token\":\"$SETUP_TOKEN\",\"organization\":{\"name\":\"Smoke ISP\",\"timezone\":\"Africa/Nairobi\"},
       \"admin\":{\"username\":\"smokeadmin\",\"email\":\"smoke@example.com\",\"full_name\":\"Smoke Admin\",\"password\":\"$ADMIN_PW\"}}")
[ "$code" = 200 ] || [ "$code" = 201 ] && ok "setup wizard completed" || fail "setup: HTTP $code $(cat /tmp/smoke-setup.json)"

TOKEN=$(curl -fsS -H 'Content-Type: application/json' -X POST "$UI/api/v1/auth/login" \
  -d "{\"username\":\"smokeadmin\",\"password\":\"$ADMIN_PW\"}" | json "['access_token']") \
  && ok "staff sign-in" || fail "staff sign-in"

code=$(curl -s -o /tmp/smoke-user.json -w '%{http_code}' -H "Authorization: Bearer ${TOKEN:-x}" -H 'Content-Type: application/json' \
  -X POST "$UI/api/v1/radius/users" -d "{\"username\":\"smokecustomer\",\"password\":\"smoke-pass-1\",\"account_expiry\":\"$(date -d '+10 days' +%F)\"}")
[ "$code" = 201 ] && ok "customer created" || fail "create customer: HTTP $code $(cat /tmp/smoke-user.json)"

# Paid for 10 more days, so FreeRADIUS should accept them, and refuse a wrong password.
reply=$(docker exec -i radius_freeradius sh -c \
  "printf 'User-Name=smokecustomer\nUser-Password=smoke-pass-1\nMessage-Authenticator=0x00\n' | radclient -t 3 -r 2 127.0.0.1:1812 auth '$RADIUS_SECRET'" 2>&1 || true)
echo "$reply" | grep -q "Access-Accept" && ok "FreeRADIUS accepts the customer" || fail "FreeRADIUS: $reply"
reply=$(docker exec -i radius_freeradius sh -c \
  "printf 'User-Name=smokecustomer\nUser-Password=wrong-pass\nMessage-Authenticator=0x00\n' | radclient -t 3 -r 2 127.0.0.1:1812 auth '$RADIUS_SECRET'" 2>&1 || true)
echo "$reply" | grep -q "Access-Reject" && ok "FreeRADIUS rejects a wrong password" || fail "FreeRADIUS wrong password: $reply"

SECRET="smokecallbacksecret$(openssl rand -hex 4)"
curl -s -o /dev/null -X POST -H 'Content-Type: application/json' -d '{}' "$UI/api/v1/pay/result/$SECRET" || true
sleep 1
if $COMPOSE logs --no-color frontend backend 2>&1 | grep -q "$SECRET"; then
  fail "the callback secret appears in the logs"
else
  ok "callback secret not in the nginx or API logs"
fi
$COMPOSE logs --no-color frontend 2>&1 | grep -q "/api/v1/pay/result/REDACTED" && ok "nginx logs the masked path" \
  || fail "nginx access log has no masked /api/v1/pay/result/REDACTED line"

# With the backend down nginx logs an upstream error, which it cannot mask.
docker stop radius_backend >/dev/null
SECRET2="smokedownsecret$(openssl rand -hex 4)"
curl -s -o /dev/null -X POST -d '{}' "$UI/api/v1/pay/confirm/$SECRET2" || true
sleep 1
$COMPOSE logs --no-color frontend 2>&1 | grep -q "$SECRET2" && fail "callback secret in nginx's error log while the backend is down" \
  || ok "callback secret not logged while the backend is down"
docker start radius_backend >/dev/null

if [ "$failed" = 1 ]; then echo "Docker smoke test FAILED"; exit 1; fi
echo "Docker smoke test passed"
