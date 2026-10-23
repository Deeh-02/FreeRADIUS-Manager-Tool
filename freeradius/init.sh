#!/bin/bash
set -e

# No fallbacks: a default shared secret or empty DB password would be a silent hole.
[ -n "${RADIUS_SECRET:-}" ] || { echo "[FreeRADIUS] RADIUS_SECRET is not set; refusing to start." >&2; exit 1; }
[ -n "${DB_PASS:-}" ] || { echo "[FreeRADIUS] DB_PASS is not set; refusing to start." >&2; exit 1; }

# RADDB and --configure-only let scripts/test-radius.sh run this same file on a test config.
RADDB="${RADDB:-/etc/freeradius}"
CONFIGURE_ONLY=false
[ "${1:-}" = "--configure-only" ] && CONFIGURE_ONLY=true

if ! $CONFIGURE_ONLY; then
  echo "[FreeRADIUS] Waiting for PostgreSQL to be ready..."
  until pg_isready -h "${DB_HOST:-postgres}" -p "${DB_PORT:-5432}" -U "${DB_USER:-radius_user}" 2>/dev/null; do
      echo "[FreeRADIUS] PostgreSQL not ready, retrying in 2s..."
      sleep 2
  done
  echo "[FreeRADIUS] PostgreSQL is ready."
fi

# Placeholders are filled in at container start (not image build) so one image works with any
# .env. The sed delimiter is "|", hence the escaping of \ & | in values below.
# Safe replacement: escapes \ & | so any password/secret can be used
sed_escape() { printf '%s' "$1" | sed 's/\\/\\\\/g; s/&/\\&/g; s/|/\\|/g'; }

# ── 1. RADIUS shared secret → clients.conf ───────────────────────────────────
SECRET="$RADIUS_SECRET"
sed -i "s|RADIUS_SECRET_PLACEHOLDER|$(sed_escape "$SECRET")|g" \
    "$RADDB/clients.conf"
echo "[FreeRADIUS] clients.conf secret set (length: ${#SECRET})."

# ── 2. DB credentials → mods-available/sql ───────────────────────────────────
SQL="$RADDB/mods-available/sql"
sed -i "s|DB_HOST_PLACEHOLDER|$(sed_escape "${DB_HOST:-postgres}")|g"    "$SQL"
sed -i "s|DB_PORT_PLACEHOLDER|$(sed_escape "${DB_PORT:-5432}")|g"        "$SQL"
sed -i "s|DB_NAME_PLACEHOLDER|$(sed_escape "${DB_NAME:-radius}")|g"      "$SQL"
sed -i "s|DB_USER_PLACEHOLDER|$(sed_escape "${DB_USER:-radius_user}")|g" "$SQL"
sed -i "s|DB_PASS_PLACEHOLDER||g"                                     "$SQL"
# The password and TLS mode reach libpq through its own environment variables rather than
# the config file: FreeRADIUS's config parser cannot hold every character a password may
# contain (quotes, backslashes), and a database elsewhere (Supabase, RDS) needs TLS
# (DB_SSL_MODE=require). The driver leaves out what is empty, so libpq uses these.
export PGPASSWORD="$DB_PASS"
[ -n "${DB_SSL_MODE:-}" ] && export PGSSLMODE="$DB_SSL_MODE" && echo "[FreeRADIUS] Database TLS: sslmode=${DB_SSL_MODE}."
# A managed database limits connections (Supabase's session pooler: about 15 on the free tier,
# shared with the backend); DB_POOL_MAX caps FreeRADIUS's share. Default 32.
if [ -n "${DB_POOL_MAX:-}" ]; then
  start=$(( DB_POOL_MAX < 5 ? DB_POOL_MAX : 5 )); min=$(( DB_POOL_MAX < 3 ? DB_POOL_MAX : 3 ))
  sed -i -e "s|^\(\s*max\s*=\s*\)32|\1${DB_POOL_MAX}|" -e "s|^\(\s*start\s*=\s*\)5|\1${start}|" \
         -e "s|^\(\s*min\s*=\s*\)3|\1${min}|" -e "s|^\(\s*spare\s*=\s*\)10|\1${min}|" "$SQL"
  echo "[FreeRADIUS] SQL connection pool capped at ${DB_POOL_MAX}."
fi
echo "[FreeRADIUS] SQL module credentials set (host: ${DB_HOST:-postgres})."

$CONFIGURE_ONLY && exit 0

echo "[FreeRADIUS] Starting FreeRADIUS..."
# exec: FreeRADIUS becomes PID 1 so `docker stop` signals reach it. The placeholders are
# replaced in place, so on a plain `docker restart` the already-substituted files are reused;
# changing RADIUS_SECRET or DB_PASS needs the container recreated, not just restarted.
exec freeradius -f -l stdout "$@"
