# syntax=docker/dockerfile:1

# --- Stage 1: Build ---
# Use the official Go image to compile a fully static binary.
# Alpine is used over Debian to keep the builder layer small.
FROM golang:1.25-alpine AS builder

WORKDIR /build

# Download dependencies first (separate layer for Docker cache efficiency).
# This layer is only invalidated when go.mod or go.sum changes, not on source changes.
COPY go.mod go.sum ./
RUN go mod download

# Copy source and build.
# CGO_ENABLED=0 produces a fully static binary with no libc dependency,
# which is required to run in the distroless runtime image below.
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o price-feed-monitor .

# --- Stage 2: Runtime ---
# distroless/static contains only CA certificates and timezone data — nothing else.
# No shell, no package manager, minimal attack surface.
# The nonroot variant runs as uid 65532 by default (no root privileges needed).
FROM gcr.io/distroless/static:nonroot

WORKDIR /app

# Copy the compiled binary from the builder stage.
COPY --from=builder /build/price-feed-monitor /app/price-feed-monitor

# The config file is NOT baked into the image. In K8s it is mounted via a ConfigMap.
# The PRICE_FEED_MONITOR_CONFIG env var tells the app where to find it.
# Default path used when running locally without the env var set.
ENV PRICE_FEED_MONITOR_CONFIG=/etc/price-feed-monitor/config.yaml

ENTRYPOINT ["/app/price-feed-monitor"]
