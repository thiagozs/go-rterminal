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

FROM debian:bookworm-slim AS base
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates bash && rm -rf /var/lib/apt/lists/*

FROM base AS server
# curl serves the container healthcheck; the server runs unprivileged and keeps
# its SQLite database in /data (mount a volume there).
RUN apt-get update && apt-get install -y --no-install-recommends curl && rm -rf /var/lib/apt/lists/* \
    && useradd --system --uid 10001 --home-dir /data --shell /usr/sbin/nologin rterminal \
    && mkdir -p /data && chown rterminal:rterminal /data
COPY --from=build /out/server /usr/local/bin/server
USER rterminal
WORKDIR /data
VOLUME /data
EXPOSE 8080

FROM base AS agent
COPY --from=build /out/agent /usr/local/bin/agent

FROM base AS client
COPY --from=build /out/client /usr/local/bin/client
