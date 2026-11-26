.PHONY: require-token run-server-admin help build build-server build-agent build-client run-server run-agent run-client healthcheck

GO ?= go
BIN_DIR ?= bin
AUTH_TOKEN ?=
DB ?= rterminal.db
PUBLIC_URL ?=
TLS_CERT ?=
TLS_KEY ?=
TLS_CA ?=
TLS_INSECURE ?= false
SESSION_TIMEOUT ?= 15s
BACKOFF_MIN ?= 5s
BACKOFF_MAX ?= 60s
KEEPALIVE_INTERVAL ?= 15s

SERVER_BIN := $(BIN_DIR)/server
AGENT_BIN := $(BIN_DIR)/agent
CLIENT_BIN := $(BIN_DIR)/client

help:
	@echo "Targets:"
	@echo "  build         Build all binaries"
	@echo "  build-server  Build server binary"
	@echo "  build-agent   Build agent binary"
	@echo "  build-client  Build client binary"
	@echo "  run-server    Run server (go run)"
	@echo "  run-server-admin  Run server with the admin panel (SQLite, no AUTH_TOKEN needed)"
	@echo "  run-agent     Run agent (go run)"
	@echo "  run-client    Run client (go run)"
	@echo "  healthcheck   Check server health endpoint"
	@echo ""
	@echo "Vars:"
	@echo "  AUTH_TOKEN   Auth token (required for run-*)"
	@echo "  TLS_CERT     Server TLS cert path"
	@echo "  TLS_KEY      Server TLS key path"
	@echo "  TLS_CA       Client/agent CA path"
	@echo "  TLS_INSECURE Client/agent skip TLS verify (true/false)"
	@echo "  SESSION_TIMEOUT Server session pairing timeout (default: 15s, 0 disables)"
	@echo "  BACKOFF_MIN  Agent backoff min (default: 5s)"
	@echo "  BACKOFF_MAX  Agent backoff max (default: 60s)"
	@echo "  KEEPALIVE_INTERVAL Agent keepalive interval (default: 15s, 0 disables)"

build: build-server build-agent build-client

build-server:
	@mkdir -p $(BIN_DIR)
	$(GO) build -o $(SERVER_BIN) ./cmd/server

build-agent:
	@mkdir -p $(BIN_DIR)
	$(GO) build -o $(AGENT_BIN) ./cmd/agent

build-client:
	@mkdir -p $(BIN_DIR)
	$(GO) build -o $(CLIENT_BIN) ./cmd/client

run-server: require-token
	$(GO) run ./cmd/server --addr :8080 --auth-token "$(AUTH_TOKEN)" \
		--session-timeout $(SESSION_TIMEOUT) \
		$(if $(TLS_CERT),--tls-cert $(TLS_CERT),) \
		$(if $(TLS_KEY),--tls-key $(TLS_KEY),)

run-server-admin:
	$(GO) run ./cmd/server --addr :8080 --db $(DB) --session-timeout $(SESSION_TIMEOUT) \
		$(if $(PUBLIC_URL),--public-url $(PUBLIC_URL),) \
		$(if $(TLS_CERT),--tls-cert $(TLS_CERT),) \
		$(if $(TLS_KEY),--tls-key $(TLS_KEY),)

run-agent: require-token
	$(GO) run ./cmd/agent --server-url ws://127.0.0.1:8080 --agent-id agent-01 \
		--shell /bin/bash --auth-token "$(AUTH_TOKEN)" \
		--backoff-min $(BACKOFF_MIN) --backoff-max $(BACKOFF_MAX) \
		--keepalive-interval $(KEEPALIVE_INTERVAL) \
		$(if $(TLS_CA),--tls-ca $(TLS_CA),) \
		$(if $(filter true,$(TLS_INSECURE)),--tls-insecure,)

run-client: require-token
	$(GO) run ./cmd/client --server-url http://127.0.0.1:8080 --agent-id agent-01 \
		--auth-token "$(AUTH_TOKEN)" \
		$(if $(TLS_CA),--tls-ca $(TLS_CA),) \
		$(if $(filter true,$(TLS_INSECURE)),--tls-insecure,)

healthcheck:
	@curl -fsS http://127.0.0.1:8080/healthz

require-token:
	@test -n "$(AUTH_TOKEN)" || { echo "defina AUTH_TOKEN (ex: make run-server AUTH_TOKEN=...)"; exit 1; }
