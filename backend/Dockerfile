# ---- Build Stage ----
FROM golang:1.21-alpine AS builder

RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /app

# Dependencies first (cached layer), pinned by the committed go.sum.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Builds for the machine running the build (amd64 or arm64).
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o radius-manager .

# ---- Runtime Stage ----
FROM alpine:3.21

# postgresql17-client provides pg_dump for the backup_database task. pg_dump refuses a server
# newer than itself, so it must be at least the database's version: the bundled postgres:15
# and managed databases such as Supabase (PostgreSQL 17 for new projects).
RUN apk add --no-cache ca-certificates tzdata curl nmap nmap-scripts postgresql17-client

WORKDIR /app

COPY --from=builder /app/radius-manager .

# nmap needs raw sockets for ARP/ICMP discovery, and backups are written to the ./backups
# host folder, so the API runs as root inside the container (plus NET_RAW/NET_ADMIN from
# docker-compose.yml). The container is not reachable from outside except through nginx.
USER root

EXPOSE 8088

HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
    CMD curl -f http://127.0.0.1:8088/health || exit 1

CMD ["./radius-manager"]
