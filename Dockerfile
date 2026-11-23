# syntax=docker/dockerfile:1

# --- Go server ---------------------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:1.26@sha256:4165ee6bf3b7811fd7b40373290405c83d0167dcb87ee5f29c427cdcef45ff8f AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY src ./src
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/creditapi ./src/cmd/creditapi

# --- Claude Code CLI (native binary, checksum verified) -----------------------
FROM debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2 AS claude
ARG TARGETARCH
# An exact version (pinned for repeatable builds), "stable" or "latest".
ARG CLAUDE_VERSION=2.1.325
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl jq \
 && rm -rf /var/lib/apt/lists/*
COPY scripts/install-claude.sh /usr/local/bin/install-claude.sh
RUN TARGETARCH=$TARGETARCH bash /usr/local/bin/install-claude.sh "$CLAUDE_VERSION" /usr/local/bin/claude

# --- Cursor CLI (cursor-agent, the package https://cursor.com/install uses) ----
FROM debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2 AS cursor
ARG TARGETARCH
# An exact version with a checksum in scripts/cursor-checksums.txt, or "none"
# for an image without Cursor support (about 540 MB smaller). "latest" needs
# --build-arg CURSOR_ALLOW_UNVERIFIED=1 unless its checksum is recorded.
ARG CURSOR_VERSION=2026.11.22-e9042b2
ARG CURSOR_ALLOW_UNVERIFIED=0
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl \
 && rm -rf /var/lib/apt/lists/*
COPY scripts/install-cursor.sh scripts/cursor-checksums.txt /usr/local/share/install/
RUN TARGETARCH=$TARGETARCH CURSOR_ALLOW_UNVERIFIED=$CURSOR_ALLOW_UNVERIFIED \
    bash /usr/local/share/install/install-cursor.sh "$CURSOR_VERSION" /opt/cursor-agent /usr/local/bin \
    /usr/local/share/install/cursor-checksums.txt

# --- Runtime -------------------------------------------------------------------
FROM debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates tini \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --create-home --uid 1000 --shell /usr/sbin/nologin app \
 && mkdir -p /data \
 && chown app:app /data
COPY --from=claude /usr/local/bin/claude /usr/local/bin/claude
COPY --from=cursor /opt/cursor-agent /opt/cursor-agent
RUN if [ -f /opt/cursor-agent/cursor-agent ]; then ln -s /opt/cursor-agent/cursor-agent /usr/local/bin/cursor-agent; fi
COPY --from=build /out/creditapi /usr/local/bin/creditapi

ENV DATA_DIR=/data \
    HOST=0.0.0.0 \
    PORT=8080 \
    DISABLE_AUTOUPDATER=1

USER app
WORKDIR /data
VOLUME ["/data"]
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
  CMD ["creditapi", "healthcheck"]
# tini reaps the CLI processes this server starts and stops.
ENTRYPOINT ["/usr/bin/tini", "--", "creditapi"]
