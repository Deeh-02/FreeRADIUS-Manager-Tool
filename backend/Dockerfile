# ---- Build Stage ----
FROM golang:1.21-alpine AS builder

RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /app

COPY go.mod ./
# Download dependencies (go.sum will be generated if missing)
RUN go mod download || true

COPY . .
# go mod tidy resolves and pins all deps, then we build. Note `tidy` may rewrite go.mod/go.sum
# inside the image, so the image can end up with dependency versions that differ from the
# committed files; the build also needs network access to the Go module proxy.
RUN go mod tidy && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-w -s" -o radius-manager ./main.go

# ---- Runtime Stage ----
FROM alpine:3.19

RUN apk add --no-cache ca-certificates tzdata curl nmap nmap-scripts

WORKDIR /app

COPY --from=builder /app/radius-manager .

# nmap needs raw socket access for some scan types. The API therefore runs as root inside
# the container (plus NET_RAW from docker-compose.yml); consider a non-root user and only
# the needed capability if the network scanner is not used.
USER root

EXPOSE 8088

HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
    CMD curl -f http://127.0.0.1:8088/health || exit 1

CMD ["./radius-manager"]
