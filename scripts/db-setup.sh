#!/usr/bin/env bash
# Prepare any PostgreSQL for this system from this checkout: a managed one (Supabase, RDS,
# Neon), a VPS, or a test database. Docker installs do not need it (Compose and
# deploy-update.sh do the same).
#
#   DATABASE_URL='postgresql://user:pass@host:5432/db?sslmode=require' TZ=Africa/Nairobi scripts/db-setup.sh
#
# 1. init-db.sql (safe to run again).
# 2. Each migrations/*.sql not yet applied, once, each in one transaction; the applied ones are
#    recorded in schema_migrations, as deploy-update.sh does.
# 3. The database's default timezone set to TZ. Most timestamps are stored without a zone, so
#    the backend and FreeRADIUS must work in the same zone; managed databases default to UTC.
#    (The backend also sets its own sessions to TZ and warns at startup if they disagree.)
#
# Needs psql (any version 13 or newer).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
URL="${DATABASE_URL:?set DATABASE_URL (postgresql://user:password@host:port/dbname?sslmode=require)}"
ZONE="${TZ:-}"
q() { psql "$URL" -v ON_ERROR_STOP=1 -q -X "$@"; }

echo "Connecting ..."
server=$(q -tAc "SHOW server_version")
echo "  PostgreSQL $server, database $(q -tAc 'SELECT current_database()'), user $(q -tAc 'SELECT current_user')"

echo "Base schema (init-db.sql) ..."
q -c "SET client_min_messages = warning" -f "$ROOT/init-db.sql" >/dev/null

q -c "SET client_min_messages = warning; CREATE TABLE IF NOT EXISTS schema_migrations (filename VARCHAR(200) PRIMARY KEY, applied_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP)"
applied=0
for f in $(ls "$ROOT"/migrations/*.sql | sort); do
  name=$(basename "$f")
  [ "$(q -tAc "SELECT 1 FROM schema_migrations WHERE filename = '$name'")" = "1" ] && continue
  echo "  migration $name"
  # The record is part of the migration's own transaction: both happen or neither.
  if ! out=$( (echo "SET client_min_messages = warning;"; cat "$f"; echo; echo "INSERT INTO schema_migrations (filename) VALUES ('$name');") | q -1 -f - 2>&1); then
    echo "$out" >&2
    echo "Migration $name failed and was rolled back. Nothing after it was applied." >&2
    exit 1
  fi
  applied=$((applied + 1))
done
echo "  $applied migration(s) applied"

if [ -n "$ZONE" ]; then
  ZONE="${ZONE#:}"
  if q -c "ALTER DATABASE \"$(q -tAc 'SELECT current_database()')\" SET timezone TO '$ZONE'" 2>/dev/null; then
    echo "Database timezone set to $ZONE (new connections)"
  else
    # Not the database's owner: set it for this login instead, which the backend and
    # FreeRADIUS share. Another login would need the same (ALTER ROLE ... SET timezone).
    q -c "ALTER ROLE CURRENT_USER SET timezone TO '$ZONE'"
    echo "Timezone set to $ZONE for login $(q -tAc 'SELECT current_user') (not the database owner, so not for the whole database)"
  fi
else
  echo "TZ not set: database timezone left at $(q -tAc 'SHOW timezone'). Set TZ=Africa/Nairobi (your zone) and run again." >&2
fi

echo "Done: $(q -tAc "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = 'public'") tables."
