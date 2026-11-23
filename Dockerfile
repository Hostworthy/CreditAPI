# syntax=docker/dockerfile:1

# --- Go server ---------------------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:1.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/claude-max-to-api ./cmd/claude-max-to-api

# --- Claude Code CLI (native binary, checksum verified) -----------------------
FROM debian:trixie-slim AS claude
ARG TARGETARCH
# "stable", "latest" or an exact version such as 2.1.323
ARG CLAUDE_VERSION=stable
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl jq \
 && rm -rf /var/lib/apt/lists/*
COPY scripts/install-claude.sh /usr/local/bin/install-claude.sh
RUN TARGETARCH=$TARGETARCH bash /usr/local/bin/install-claude.sh "$CLAUDE_VERSION" /usr/local/bin/claude

# --- Runtime -------------------------------------------------------------------
FROM debian:trixie-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates tini \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --create-home --uid 1000 --shell /usr/sbin/nologin app \
 && mkdir -p /data \
 && chown app:app /data
COPY --from=claude /usr/local/bin/claude /usr/local/bin/claude
COPY --from=build /out/claude-max-to-api /usr/local/bin/claude-max-to-api

ENV DATA_DIR=/data \
    LISTEN_ADDR=:8080 \
    DISABLE_AUTOUPDATER=1

USER app
WORKDIR /data
VOLUME ["/data"]
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
  CMD ["claude-max-to-api", "healthcheck"]
# tini reaps the claude processes this server starts and stops.
ENTRYPOINT ["/usr/bin/tini", "--", "claude-max-to-api"]
