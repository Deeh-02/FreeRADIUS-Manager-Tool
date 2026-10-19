# Use pinned FreeRADIUS 3.2.10
FROM freeradius/freeradius-server:3.2.10

USER root

# postgresql-client: pg_isready in init.sh. tzdata: lets TZ from docker-compose.yml take
# effect, since the Expiration check compares against FreeRADIUS's local time.
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        postgresql-client tzdata \
    && rm -rf /var/lib/apt/lists/*

# ── Overwrite config files at the CORRECT path: /etc/freeradius/ ─────────────
COPY radiusd.conf                              /etc/freeradius/radiusd.conf
COPY clients.conf                              /etc/freeradius/clients.conf
COPY dictionary                                /etc/freeradius/dictionary
COPY sites-available/default                   /etc/freeradius/sites-available/default
COPY sites-available/inner-tunnel              /etc/freeradius/sites-available/inner-tunnel
COPY mods-available/sql                        /etc/freeradius/mods-available/sql
COPY mods-available/quota                      /etc/freeradius/mods-available/quota
COPY mods-config/sql/main/postgresql/queries.conf \
     /etc/freeradius/mods-config/sql/main/postgresql/queries.conf

COPY init.sh /init.sh

# Enable the SQL and quota modules (symlinks in mods-enabled)
RUN ln -sf /etc/freeradius/mods-available/sql \
           /etc/freeradius/mods-enabled/sql && \
    ln -sf /etc/freeradius/mods-available/quota \
           /etc/freeradius/mods-enabled/quota && \
    chmod +x /init.sh

EXPOSE 1812/udp 1813/udp

# Fails if FreeRADIUS stops answering: Status-Server is enabled in radiusd.conf.
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
    CMD echo "Message-Authenticator = 0x00" | radclient -t 2 -r 1 127.0.0.1:1812 status "$RADIUS_SECRET" >/dev/null 2>&1 || exit 1

ENTRYPOINT ["/init.sh"]
