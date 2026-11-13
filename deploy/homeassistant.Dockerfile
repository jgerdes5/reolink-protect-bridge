# syntax=docker/dockerfile:1
# Home Assistant add-on image (reolink_protect_bridge/config.yaml), built from
# the repository root by the release workflow, one image per add-on arch.

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

FROM debian:trixie-slim
ARG VERSION=dev
# add-on version as in reolink_protect_bridge/config.yaml (tag without "v")
ARG BUILD_VERSION=dev
ARG BUILD_ARCH
RUN apt-get update \
 && apt-get install -y --no-install-recommends ffmpeg ca-certificates \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/reolink-protect-bridge /usr/local/bin/reolink-protect-bridge

LABEL io.hass.type="addon" \
      io.hass.name="Reolink Protect Bridge" \
      io.hass.version="${BUILD_VERSION}" \
      io.hass.arch="${BUILD_ARCH}"

# Home Assistant mounts the add-on's persistent folder at /data.
ENV RPB_DATA=/data RPB_INGRESS=1
ENTRYPOINT ["/usr/local/bin/reolink-protect-bridge"]
