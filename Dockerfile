FROM --platform=$BUILDPLATFORM golang:1.27.0 AS build
ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -ldflags "-X main.version=$VERSION" -o /out/server ./cmd/server
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -ldflags "-X main.version=$VERSION" -o /out/agent ./cmd/agent
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -ldflags "-X main.version=$VERSION" -o /out/client ./cmd/client
# Agent binaries the server hands out at /download/agent/<os>-<arch>. The agent
# is static Go, so one build host cross-compiles every platform (GOARM only
# affects 32-bit ARM: v6 runs on Raspberry Pi Zero through current models).
RUN set -eu; mkdir -p /out/agents; \
    for target in linux/amd64 linux/arm64 linux/arm darwin/amd64 darwin/arm64; do \
      os=${target%/*}; arch=${target#*/}; \
      CGO_ENABLED=0 GOOS=$os GOARCH=$arch GOARM=6 go build -trimpath \
        -ldflags "-s -w -X main.version=$VERSION" -o /out/agents/agent-$os-$arch ./cmd/agent; \
    done

FROM debian:bookworm-slim AS base
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates bash && rm -rf /var/lib/apt/lists/*

FROM base AS server
# curl serves the container healthcheck; the server runs unprivileged and keeps
# its SQLite database in /data (mount a volume there).
RUN apt-get update && apt-get install -y --no-install-recommends curl && rm -rf /var/lib/apt/lists/* \
    && useradd --system --uid 10001 --home-dir /data --shell /usr/sbin/nologin rterminal \
    && mkdir -p /data && chown rterminal:rterminal /data
COPY --from=build /out/server /usr/local/bin/server
COPY --from=build /out/agents /usr/share/rterminal/agents
ENV RTERMINAL_AGENTS_DIR=/usr/share/rterminal/agents
USER rterminal
WORKDIR /data
VOLUME /data
EXPOSE 8080

FROM base AS agent
COPY --from=build /out/agent /usr/local/bin/agent

FROM base AS client
COPY --from=build /out/client /usr/local/bin/client
