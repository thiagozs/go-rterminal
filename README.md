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
go run ./cmd/server --addr :8080 --auth-token change-me
```

2. Subir o agent:

```bash
go run ./cmd/agent --server-url ws://127.0.0.1:8080 --agent-id agent-01 --shell /bin/bash --auth-token change-me
```

3. Subir o client:

```bash
go run ./cmd/client --server-url http://127.0.0.1:8080 --agent-id agent-01 --auth-token change-me
```

## Flags disponiveis

### server

- `--addr` endereco de escuta HTTP (ex: `:8080`)
- `--auth-token` token de autenticacao para agentes e clientes
- `--tls-cert` caminho do certificado TLS (opcional)
- `--tls-key` caminho da chave TLS (opcional)
- `--session-timeout` timeout para pareamento de sessao (padrao `15s`, `0` desativa)

### agent

- `--server-url` URL WS do server (ex: `ws://127.0.0.1:8080`)
- `--agent-id` ID do agent (ex: `agent-01`)
- `--shell` shell para abrir no agente (ex: `/bin/bash`)
- `--auth-token` token de autenticacao
- `--tls-ca` caminho do CA para TLS (opcional)
- `--tls-insecure` ignora verificacao TLS (nao recomendado)
- `--backoff-min` backoff minimo para reconexao (padrao `5s`)
- `--backoff-max` backoff maximo para reconexao (padrao `60s`)
- `--keepalive-interval` intervalo de keepalive no controle (padrao `15s`, `0` desativa)

### client

- `--server-url` URL HTTP do server (ex: `http://127.0.0.1:8080`)
- `--agent-id` ID do agent alvo (ex: `agent-01`)
- `--auth-token` token de autenticacao
- `--tls-ca` caminho do CA para TLS (opcional)
- `--tls-insecure` ignora verificacao TLS (nao recomendado)

## Healthcheck

- `GET /healthz` retorna `200 OK` com corpo `ok`

## O que este projeto representa

Este projeto e uma base enxuta para estudar e testar:

- pareamento de clientes e agentes via WebSocket
- multiplexacao simples de sessoes
- comunicacao de terminal em modo raw com PTY

Ele inclui autenticacao simples por token e suporte opcional a TLS, mas nao faz autorizacao detalhada. Use apenas em ambientes controlados.
