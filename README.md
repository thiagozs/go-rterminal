# go-shellhub

go-shellhub e um projeto simples para criar sessoes de shell remotas via WebSocket. Ele e dividido em tres componentes:

- server: coordena agentes e clientes e cria sessoes
- agent: conecta ao server e abre um shell local em PTY
- client: conecta ao server e interage com a sessao

O fluxo e: o client pede uma nova sessao ao server, o server aciona o agent, e a comunicacao passa a fluir entre client e agent.

## Requisitos

- Go 1.25+ (conforme `go.mod`)
- Acesso local a `127.0.0.1:8080` (ajuste via flags se necessario)

## Como executar

1. Subir o server:

```bash
go run ./cmd/server --addr :8080
```

2. Subir o agent:

```bash
go run ./cmd/agent --server-url ws://127.0.0.1:8080 --agent-id agent-01 --shell /bin/bash
```

3. Subir o client:

```bash
go run ./cmd/client --server-url http://127.0.0.1:8080 --agent-id agent-01
```

## Flags disponiveis

### server

- `--addr` endereco de escuta HTTP (ex: `:8080`)

### agent

- `--server-url` URL WS do server (ex: `ws://127.0.0.1:8080`)
- `--agent-id` ID do agent (ex: `agent-01`)
- `--shell` shell para abrir no agente (ex: `/bin/bash`)

### client

- `--server-url` URL HTTP do server (ex: `http://127.0.0.1:8080`)
- `--agent-id` ID do agent alvo (ex: `agent-01`)

## O que este projeto representa

Este projeto e uma base enxuta para estudar e testar:

- pareamento de clientes e agentes via WebSocket
- multiplexacao simples de sessoes
- comunicacao de terminal em modo raw com PTY

Ele nao inclui autenticacao, autorizacao ou criptografia. Use apenas em ambientes controlados.
