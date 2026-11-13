# syntax=docker/dockerfile:1

# ---- build ------------------------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS TARGETARCH TARGETVARIANT
ARG VERSION=dev
WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    GOARM="$(echo "$TARGETVARIANT" | tr -d v)" \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" \
      -o /out/reolink-protect-bridge ./cmd/reolink-protect-bridge

# ---- runtime ----------------------------------------------------------------
FROM debian:trixie-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ffmpeg ca-certificates wget \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --system --uid 10001 --home-dir /data --shell /usr/sbin/nologin rpb \
 && mkdir -p /data && chown rpb:rpb /data

COPY --from=build /out/reolink-protect-bridge /usr/local/bin/reolink-protect-bridge

ENV RPB_DATA=/data
VOLUME /data
# Web UI. The container is meant to run with network_mode: host (talkback
# arrives on UDP 7004 from the UniFi console, discovery uses broadcast), so
# EXPOSE is documentation only.
EXPOSE 8080
USER rpb

HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD wget -q -O /dev/null http://127.0.0.1:8080/ || exit 1

ENTRYPOINT ["/usr/local/bin/reolink-protect-bridge"]
