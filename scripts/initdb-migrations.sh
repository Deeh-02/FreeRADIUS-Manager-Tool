#!/usr/bin/env bash
# Runs once, when Compose starts PostgreSQL with an empty data volume (mounted into
# docker-entrypoint-initdb.d after init-db.sql). Applies every migrations/*.sql in order and
# records each in schema_migrations in the same transaction, the way deploy-update.sh does
# later. Without the records the updater would apply them all again, and the ones that set a
# default (012: currency KES, 014: 5-day trial) would undo what staff chose on Settings.
set -euo pipefail
dir="${MIGRATIONS_DIR:-/migrations}"
q() { psql -v ON_ERROR_STOP=1 -q -X --username "${POSTGRES_USER:-postgres}" --dbname "${POSTGRES_DB:-postgres}" "$@"; }
q -c "SET client_min_messages = warning; CREATE TABLE IF NOT EXISTS schema_migrations (filename VARCHAR(200) PRIMARY KEY, applied_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP)"
for f in $(ls "$dir"/*.sql | sort); do
  name=$(basename "$f")
  echo "initdb: migration $name"
  (echo "SET client_min_messages = warning;"; cat "$f"; echo; echo "INSERT INTO schema_migrations (filename) VALUES ('$name') ON CONFLICT DO NOTHING;") | q -1 -f -
done
