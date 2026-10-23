#!/usr/bin/env bash
# Throwaway FreeRADIUS for the end-to-end tests, run as your own user (no root, no Docker).
#
#   scripts/test-radius.sh start   build a config like the Docker image's (stock raddb for the
#                                  installed version + this repo's freeradius/ files), point
#                                  it at the scratch database from scripts/test-db.sh, start it
#                                  and print the TEST_RADIUS_* variables to use
#   scripts/test-radius.sh stop    stop it and delete the config
#   scripts/test-radius.sh restart stop and start the same config (failure tests); also
#                                  down / up (stopped until "up"), pause / resume (SIGSTOP:
#                                  the process is there but never answers)
#
# Pointing it at another database (a cloud test against Supabase): TEST_RADIUS_DB_HOST,
# TEST_PG_PORT, TEST_PG_DB, TEST_RADIUS_DB_USER, TEST_RADIUS_DB_PASS, TEST_RADIUS_DB_SSLMODE,
# TEST_RADIUS_POOL_MAX; TEST_RADIUS_LISTEN=0.0.0.0 lets a router on your network reach it.
#
# Needs the freeradius package (for the binary and modules); the stock config is downloaded
# once from GitHub because /etc/freeradius is readable only by root. It listens on
# 127.0.0.1 only, on TEST_RADIUS_PORT (auth) and TEST_RADIUS_PORT+1 (accounting).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DIR="${TEST_RADIUS_DIR:-/tmp/radius-manager-test-radius}"
CACHE="${TEST_RADIUS_CACHE:-/tmp/radius-manager-raddb-cache}"
PORT="${TEST_RADIUS_PORT:-18120}"
SECRET="${TEST_RADIUS_SECRET:-testing123-e2e}"
PG_PORT="${TEST_PG_PORT:-54329}"
PG_DB="${TEST_PG_DB:-radius_test}"
RAD="$DIR/raddb"
# What init.sh exports in the container: libpq reads the password and TLS mode from here.
export PGPASSWORD="${TEST_RADIUS_DB_PASS:-unused}"
[ -n "${TEST_RADIUS_DB_SSLMODE:-}" ] && export PGSSLMODE="$TEST_RADIUS_DB_SSLMODE"

command -v freeradius >/dev/null || { echo "freeradius not installed (apt install freeradius freeradius-postgresql)" >&2; exit 1; }

pid() { cat "$DIR/run/radiusd.pid" 2>/dev/null || true; }
running() { local p; p="$(pid)"; [ -n "$p" ] && kill -0 "$p" 2>/dev/null; }

alive() {
  echo "Message-Authenticator = 0x00" | radclient -q -t 1 -r 1 "127.0.0.1:$PORT" status "$SECRET" >/dev/null 2>&1
}

build() {
  ver="$(freeradius -v | sed -n 's/.*Version \([0-9.]*\).*/\1/p' | head -1)"
  tag="release_${ver//./_}"
  if [ ! -d "$CACHE/$tag/raddb" ]; then
    mkdir -p "$CACHE/$tag"
    curl -fsSL "https://github.com/FreeRADIUS/freeradius-server/archive/refs/tags/$tag.tar.gz" |
      tar xz -C "$CACHE/$tag" --strip-components=1 "freeradius-server-$tag/raddb"
  fi
  rm -rf "$DIR"
  mkdir -p "$DIR/log" "$DIR/run"
  cp -r "$CACHE/$tag/raddb" "$RAD"
  rm -f "$RAD"/*.in

  # This repo's files, at the same paths the Dockerfile copies them to.
  F="$ROOT/freeradius"
  cp "$F/radiusd.conf" "$F/clients.conf" "$F/dictionary" "$RAD/"
  cp "$F/sites-available/default" "$F/sites-available/inner-tunnel" "$RAD/sites-available/"
  cp "$F/mods-available/sql" "$F/mods-available/quota" "$RAD/mods-available/"
  cp "$F/mods-config/sql/main/postgresql/queries.conf" "$RAD/mods-config/sql/main/postgresql/"

  # Modules and sites the package enables by default (raddb/all.mk), plus sql and quota.
  mkdir -p "$RAD/mods-enabled" "$RAD/sites-enabled"
  for m in always attr_filter chap date detail detail.log digest dynamic_clients eap echo exec \
           expiration expr files linelog logintime mschap ntlm_auth pap passwd preprocess \
           radutmp realm replicate soh sradutmp totp unix unpack utf8 sql quota; do
    [ -e "$RAD/mods-available/$m" ] && ln -sf "../mods-available/$m" "$RAD/mods-enabled/$m"
  done
  for s in default inner-tunnel; do ln -sf "../sites-available/$s" "$RAD/sites-enabled/$s"; done

  # Run from this directory as this user, on our ports.
  sed -i -e "s|^raddbdir = .*|raddbdir = $RAD|" -e "s|^logdir = .*|logdir = $DIR/log|" \
         -e "s|^run_dir = .*|run_dir = $DIR/run|" -e "s|^pidfile = .*|pidfile = $DIR/run/radiusd.pid|" \
         -e '/^\s*user = /d' -e '/^\s*group = /d' -e 's|colourise = yes|colourise = no|' \
         -e "s|destination = stdout|destination = files|" "$RAD/radiusd.conf"
  sed -i -e "s|port   = 1812|port   = $PORT|" -e "s|port   = 1813|port   = $((PORT + 1))|" \
         -e "s|ipaddr = \*|ipaddr = ${TEST_RADIUS_LISTEN:-127.0.0.1}|" "$RAD/sites-available/default"
  sed -i -e "s|port = 18120|port = $((PORT + 2))|" "$RAD/sites-available/inner-tunnel"
  sed -i -e "s|/etc/freeradius/|$RAD/|g" "$RAD/mods-available/sql"

  # The container's own init.sh fills in the secret and database settings, so the tests run
  # exactly that code. TEST_RADIUS_DB_USER/PASS/SSLMODE try other logins (TLS, odd passwords).
  RADIUS_SECRET="$SECRET" DB_HOST="${TEST_RADIUS_DB_HOST:-127.0.0.1}" DB_PORT="$PG_PORT" DB_NAME="$PG_DB" \
    DB_POOL_MAX="${TEST_RADIUS_POOL_MAX:-}" \
    DB_USER="${TEST_RADIUS_DB_USER:-postgres}" DB_PASS="${TEST_RADIUS_DB_PASS:-unused}" \
    DB_SSL_MODE="${TEST_RADIUS_DB_SSLMODE:-}" RADDB="$RAD" \
    bash "$ROOT/freeradius/init.sh" --configure-only >"$DIR/log/init.log" 2>&1 \
    || { cat "$DIR/log/init.log" >&2; exit 1; }

  # EAP needs certificates; the test ones from the stock bootstrap script are fine here.
  (cd "$RAD/certs" && ./bootstrap >/dev/null 2>&1) || { echo "certificate bootstrap failed" >&2; exit 1; }

  freeradius -C -d "$RAD" >"$DIR/log/check.log" 2>&1 || { cat "$DIR/log/check.log" >&2; exit 1; }
}

launch() {
  # Optional fake clock (midnight tests): TEST_RADIUS_FAKETIME_LIB=/path/libfaketimeMT.so.1.
  # The time is read from $DIR/faketime on every call ("@2026-10-23 23:59:57" = start there and
  # run), so a test can move it without a restart. TZ from the environment is FreeRADIUS's zone.
  if [ -n "${TEST_RADIUS_FAKETIME_LIB:-}" ]; then
    [ -f "$DIR/faketime" ] || echo "+0" >"$DIR/faketime"
    LD_PRELOAD="$TEST_RADIUS_FAKETIME_LIB" FAKETIME_TIMESTAMP_FILE="$DIR/faketime" FAKETIME_NO_CACHE=1 \
      freeradius -d "$RAD" -l "$DIR/log/radius.log"
  else
    freeradius -d "$RAD" -l "$DIR/log/radius.log"
  fi
  for _ in $(seq 1 50); do alive && return 0; sleep 0.2; done
  echo "FreeRADIUS did not answer; log:" >&2
  tail -30 "$DIR/log/radius.log" >&2
  exit 1
}

halt() {
  local p; p="$(pid)"
  if [ -n "$p" ]; then
    kill "$p" 2>/dev/null || true
    for _ in $(seq 1 50); do kill -0 "$p" 2>/dev/null || break; sleep 0.1; done
  fi
}

case "${1:-}" in
  start)
    if running; then echo "already running" >&2; else build; launch; fi
    echo "export TEST_RADIUS_ADDR=127.0.0.1:$PORT TEST_RADIUS_SECRET=$SECRET"
    ;;
  restart)
    halt; launch
    ;;
  down)    # stop, keeping the config for "up"
    halt
    ;;
  up)
    running || launch
    ;;
  pause)   # stop answering without losing state (SIGSTOP): an unreachable RADIUS server
    kill -STOP "$(pid)"
    ;;
  resume)
    kill -CONT "$(pid)"
    ;;
  stop)
    halt
    rm -rf "$DIR"
    ;;
  log)
    tail -n "${2:-50}" "$DIR/log/radius.log"
    ;;
  *)
    echo "usage: $0 start|stop|restart|down|up|pause|resume|log [lines]" >&2
    exit 2
    ;;
esac
