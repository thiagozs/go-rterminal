# go-rterminal

go-rterminal e um projeto simples para criar sessoes de shell remotas via WebSocket. Ele e dividido em tres componentes:

- server: coordena agentes e clientes e cria sessoes
- agent: conecta ao server e abre um shell local em PTY
- client: conecta ao server e interage com a sessao

O fluxo e: o client pede uma nova sessao ao server, o server aciona o agent, e a comunicacao passa a fluir entre client e agent.

## Arquitetura

```text
client -- HTTP /client --> server -- controle WS --> agent
client <======== WebSocket /session ========> agent (via relay do server)
                                                |
                                               PTY
                                                |
                                              shell
```

O codigo compartilhado fica em `internal/`:

- `protocol`: mensagens e tipos do protocolo
- `relay`: estado, autenticacao, pareamento e relay do servidor
- `endpoint`: montagem e validacao das URLs HTTP/WS
- `tlsutil`: configuracao TLS comum ao client e agent
- `backoff`: politica de reconexao do agent

## Requisitos

- Go 1.25+ (conforme `go.mod`)
- Acesso local a `127.0.0.1:8080` (ajuste via flags se necessario)
- Sistemas suportados:
  - `server`: Linux, macOS, Windows
  - `client`: Linux, macOS, Windows
  - `agent`: Linux, macOS (usa PTY; nao funciona no Windows)

## Como executar

O token e obrigatorio e `change-me` e rejeitado. Gere um (ex: `export TOKEN=$(openssl rand -hex 32)`) ou use a variavel `RTERMINAL_TOKEN`, que evita expor o token em `ps`.

1. Subir o server:

```bash
go run ./cmd/server --addr :8080 --auth-token "$TOKEN"
```

2. Subir o agent:

```bash
go run ./cmd/agent --server-url ws://127.0.0.1:8080 --agent-id agent-01 --shell /bin/bash --auth-token "$TOKEN"
```

3. Subir o client:

```bash
go run ./cmd/client --server-url http://127.0.0.1:8080 --agent-id agent-01 --auth-token "$TOKEN"
```

## Flags disponiveis

### server

- `--addr` endereco de escuta HTTP (ex: `:8080`)
- `--auth-token` token de autenticacao (obrigatorio; ou `RTERMINAL_TOKEN`)
- `--tls-cert` caminho do certificado TLS (opcional)
- `--tls-key` caminho da chave TLS (opcional)
- `--session-timeout` timeout para pareamento de sessao (padrao `15s`, `0` desativa)

### agent

- `--server-url` URL WS do server (ex: `ws://127.0.0.1:8080`)
- `--agent-id` ID do agent (ex: `agent-01`)
- `--shell` shell para abrir no agente (ex: `/bin/bash`)
- `--auth-token` token de autenticacao (obrigatorio; ou `RTERMINAL_TOKEN`)
- `--tls-ca` caminho do CA para TLS (opcional)
- `--tls-insecure` ignora verificacao TLS (nao recomendado)
- `--backoff-min` backoff minimo para reconexao (padrao `5s`)
- `--backoff-max` backoff maximo para reconexao (padrao `60s`)
- `--keepalive-interval` intervalo de keepalive no controle (padrao `15s`, `0` desativa)
- `--max-sessions` maximo de sessoes de shell simultaneas (padrao `8`, `0` sem limite)

### client

- `--server-url` URL HTTP do server (ex: `http://127.0.0.1:8080`)
- `--agent-id` ID do agent alvo (ex: `agent-01`)
- `--auth-token` token de autenticacao (obrigatorio; ou `RTERMINAL_TOKEN`)
- `--tls-ca` caminho do CA para TLS (opcional)
- `--tls-insecure` ignora verificacao TLS (nao recomendado)

## Healthcheck

- `GET /healthz` retorna `200 OK` com corpo `ok`

## O que este projeto representa

Este projeto e uma base enxuta para estudar e testar:

- pareamento de clientes e agentes via WebSocket
- multiplexacao simples de sessoes
- comunicacao de terminal em modo raw com PTY
- propagacao do tamanho inicial do terminal para o PTY remoto

Ele inclui autenticacao simples por token e suporte opcional a TLS, mas nao faz autorizacao por usuario ou por agent, auditoria, isolamento do shell ou criptografia propria. Troque o token padrao e use `wss`/`https` fora de uma rede local confiavel. O agent concede ao usuario remoto os mesmos privilegios do processo que o executa; use uma conta dedicada e com privilegios minimos.
