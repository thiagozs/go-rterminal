FROM golang:1.25.1 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/server ./cmd/server
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/agent ./cmd/agent
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/client ./cmd/client

FROM debian:bookworm-slim AS base
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates bash && rm -rf /var/lib/apt/lists/*

FROM base AS server
COPY --from=build /out/server /usr/local/bin/server
EXPOSE 8080

FROM base AS agent
COPY --from=build /out/agent /usr/local/bin/agent

FROM base AS client
COPY --from=build /out/client /usr/local/bin/client
