.PHONY: help build build-server build-agent build-client run-server run-agent run-client

GO ?= go
BIN_DIR ?= bin

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
	@echo "  run-agent     Run agent (go run)"
	@echo "  run-client    Run client (go run)"

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

run-server:
	$(GO) run ./cmd/server --addr :8080

run-agent:
	$(GO) run ./cmd/agent --server-url ws://127.0.0.1:8080 --agent-id agent-01 --shell /bin/bash

run-client:
	$(GO) run ./cmd/client --server-url http://127.0.0.1:8080 --agent-id agent-01
