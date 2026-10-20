#!/usr/bin/env bash
# Throwaway PostgreSQL for the integration tests (backend/internal/handlers/*_test.go).
#
#   scripts/test-db.sh start   create + start a scratch cluster, load the schema, print the
#                              TEST_DATABASE_URL to use
#   scripts/test-db.sh stop    stop it and delete the data
#
# Needs the PostgreSQL server binaries on this machine (apt install postgresql). It listens
# on 127.0.0.1 only, uses no password, and is never the production database.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DIR="${TEST_PG_DIR:-/tmp/radius-manager-test-pg}"
PORT="${TEST_PG_PORT:-54329}"
BIN="${PG_BIN:-$(ls -d /usr/lib/postgresql/*/bin 2>/dev/null | sort -V | tail -1)}"
URL="host=127.0.0.1 port=$PORT user=postgres dbname=radius_test sslmode=disable"

[ -x "$BIN/pg_ctl" ] || { echo "PostgreSQL server binaries not found (set PG_BIN=/path/to/bin)" >&2; exit 1; }
psql_t() { psql -h 127.0.0.1 -p "$PORT" -U postgres -v ON_ERROR_STOP=1 -q "$@"; }

case "${1:-}" in
  start)
    if "$BIN/pg_isready" -h 127.0.0.1 -p "$PORT" >/dev/null 2>&1; then
      echo "already running on port $PORT"
    else
      rm -rf "$DIR"
      "$BIN/initdb" -D "$DIR" -U postgres --auth=trust >/dev/null
      # fsync off: the data is disposable and the tests are write-heavy.
      "$BIN/pg_ctl" -D "$DIR" -l "$DIR/log" -w \
        -o "-p $PORT -c unix_socket_directories= -c fsync=off -c max_connections=200" start >/dev/null
      psql_t -c "CREATE DATABASE radius_test"
      psql_t -d radius_test -f "$ROOT/init-db.sql" >/dev/null 2>&1
      for f in "$ROOT"/migrations/*.sql; do
        case "$f" in *008_network_scanner.sql) continue ;; esac  # already part of init-db.sql
        psql_t -d radius_test -f "$f" >/dev/null 2>&1
      done
    fi
    echo "export TEST_DATABASE_URL=\"$URL\""
    ;;
  stop)
    "$BIN/pg_ctl" -D "$DIR" -m immediate stop >/dev/null 2>&1 || true
    rm -rf "$DIR"
    ;;
  *)
    echo "usage: $0 start|stop" >&2
    exit 2
    ;;
esac
