![go-rterminal: remote shells over WebSocket](docs/images/banner.png)

# go-rterminal

go-rterminal e um projeto simples para criar sessoes de shell remotas via WebSocket. Ele e dividido em tres componentes:

- **server**: coordena agentes e clientes, autentica, pareia as pontas e faz o relay dos bytes
- **agent**: conecta ao server e abre um shell local em PTY quando um cliente pede
- **client**: conecta ao server e interage com a sessao em modo raw, com resize do terminal

Opcionalmente o server tambem serve um **painel de administracao** web (veja [Painel de administracao](#painel-de-administracao)): cadastro de maquinas e grupos, geracao do comando de instalacao do agent, status online/offline, terminal no navegador e auditoria.

O fluxo e: o client pede uma nova sessao ao server, o server aciona o agent, os dois entram na sessao, o server avisa que a sessao esta pareada, e a comunicacao passa a fluir entre client e agent.

## Arquitetura

![Diagrama: client, server, agent, PTY e shell, com os passos de criacao de uma sessao](docs/images/architecture.png)

Os numeros do diagrama sao os mesmos do fluxo abaixo:

```mermaid
sequenceDiagram
    autonumber
    participant C as client
    participant S as server (relay)
    participant A as agent
    participant P as PTY / shell

    C->>S: POST /client?agent=ID (Bearer token)
    S->>A: start-session {session_id} (WS de controle /agent)
    S-->>C: 200 session_id
    C->>S: WS /session?id=session_id
    A->>S: WS /session?id=session_id
    S-->>C: paired
    S-->>A: paired
    alt agent com slot livre
        A->>P: abre o shell
        C-->>P: bytes + resize (via relay)
        P-->>C: saida do shell (via relay)
    else limite de --max-sessions atingido
        A-->>C: close 1013 "agent busy: max sessions reached"
    end
```

O codigo compartilhado fica em `internal/`:

- `protocol`: mensagens e tipos do protocolo (`register-agent`, `start-session`, `paired`, `resize`)
- `relay`: estado, autenticacao, pareamento, keepalive e relay do servidor
- `store`: persistencia em SQLite (admins, grupos, maquinas, auditoria)
- `admin`: API REST, login do administrador, terminal web e a interface embutida (`internal/admin/web`)
- `auth`: resolucao do token (flag ou `RTERMINAL_TOKEN`) e header `Authorization`
- `endpoint`: montagem e validacao das URLs HTTP/WS
- `tlsutil`: configuracao TLS comum ao client e agent
- `backoff`: politica de reconexao do agent

### Endpoints do server

| Endpoint | Metodo | Quem usa | Descricao |
| --- | --- | --- | --- |
| `/agent` | WebSocket | agent | registra o agent (`register-agent`) e mantem o canal de controle |
| `/client?agent=ID` | `POST` | client | pede uma sessao ao agent; retorna o `session_id` |
| `/session?id=ID` | WebSocket | client e agent | as duas pontas entram na mesma sessao e o server faz o relay |
| `/healthz` | `GET` | operacao | retorna `200 OK` com corpo `ok` |

Todos exigem `Authorization: Bearer <token>`, exceto `/healthz`. No modo legado o token e o mesmo para agents e clientes; no modo com painel (`--db`), cada agent usa a credencial da propria maquina (veja abaixo).

## Requisitos

- Go 1.27+ (conforme `go.mod`)
- Acesso local a `127.0.0.1:8080` (ajuste via flags se necessario)
- Sistemas suportados:
  - `server`: Linux, macOS, Windows
  - `client`: Linux, macOS, Windows (sem resize automatico no Windows)
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

Tambem da para usar o `Makefile` (`make help`) ou o Docker Compose:

```bash
export AUTH_TOKEN=$(openssl rand -hex 32)
docker compose up --build            # server + agent
docker compose run --rm client       # client interativo
```

## Painel de administracao

Passando `--db` ao server, ele passa a guardar maquinas e grupos em SQLite e serve uma interface web na mesma porta. Exemplo para um homelab (notebook, Raspberry e um servidor remoto):

```bash
go run ./cmd/server --addr :8080 --db rterminal.db --public-url https://rt.exemplo.com
```

**Primeiro acesso.** Quando o banco ainda nao tem administrador, o server cria o usuario `admin` (`--admin-user`) com uma **senha temporaria aleatoria** e a mostra nos logs, uma unica vez:

```text
[server] PRIMEIRO ACESSO: administrador criado com senha TEMPORARIA
[server]   usuario: admin
[server]   senha:   ANSdnHGbR4b4A-uqvIUkObVB
```

Entre em `http://localhost:8080/` com ela: a interface pede uma senha nova (minimo de 8 caracteres) e nada mais funciona ate a troca. A troca encerra as outras sessoes do usuario. Depois, **Alterar senha** no topo da tela faz o mesmo quando quiser.

- Se preferir definir a senha inicial, use `RTERMINAL_ADMIN_PASSWORD` (sem troca obrigatoria).
- **Perdeu a senha?** Gere outra temporaria, com troca obrigatoria, no proprio banco: `server reset-password --db rterminal.db --user admin` (em Docker: `docker exec rterminal-server /usr/local/bin/server reset-password --db /data/rterminal.db --user admin`). As sessoes abertas sao encerradas.

1. Crie o grupo (ex: `homelab`) em **Grupos > + Novo**.
2. Clique em **+ Adicionar maquina**, informe o nome (ex: `Raspberry Pi`) e o grupo.
3. A interface mostra o comando para rodar na maquina: **Linux (systemd)**, que instala o agent como servico, ou **Executar agora**, que roda em primeiro plano (Linux ou macOS). Ele carrega a **credencial da maquina**, exibida so uma vez.
4. Rode o comando na maquina. Ele **baixa o agent do proprio server**, ja para a arquitetura da maquina (veja [Download do agent](#download-do-agent)). Ela aparece como **online**, com hostname, SO e arquitetura, e o botao **Terminal** abre um shell no navegador.

Use `--public-url` para o endereco externo correto (principalmente atras de proxy reverso); sem ele, usa o host da requisicao.

Como funciona:

- **Credencial por maquina**: guardada so como hash (SHA-256). O `agent-id` vem da credencial, entao um agent nao consegue se passar por outro. **Nova credencial** invalida a antiga e derruba o agent conectado; **Excluir** tambem o desconecta.
- **Login do administrador**: senha com bcrypt, cookie `HttpOnly` + `SameSite=Strict` (e `Secure` com HTTPS), HSTS quando o `--public-url` e https, limite de tentativas por IP e por usuario (tambem na troca de senha), e protecao CSRF (cabecalho `X-Requested-With` em toda escrita). O WebSocket do terminal recusa origens diferentes.
- **Auditoria**: logins, falhas de login, mudancas em grupos e maquinas, rotacao de credenciais e cada terminal aberto.
- **Clientes CLI**: com `--db`, `--auth-token` passa a ser opcional e so autentica o client de linha de comando, com acesso a qualquer maquina online. Sem ele, so o painel abre sessoes.

Limites desta versao: ha um unico papel (administrador); os grupos organizam as maquinas, mas ainda nao restringem acesso. Nao ha rede entre as maquinas (IP, ping): isso e outro produto. Use HTTPS (`--tls-cert`/`--tls-key` ou um proxy como acima): sem TLS, a senha e as credenciais trafegam em texto puro, e o server avisa no log.

### Varios terminais: abas, divisao e popup

O botao **Terminal** abre o espaco de terminais. Cada terminal e uma sessao independente (shell proprio, WebSocket proprio) e o agent aceita varias ao mesmo tempo por maquina.

- **Abas**: use **+ Novo terminal...** para abrir outra sessao, na mesma maquina ou em outra (so as online aparecem). Repetidas viram `Maquina #2`, `#3`. O ponto verde mostra a sessao conectada; o `✕` fecha so aquele terminal.
- **Disposicao**: o seletor alterna entre **Abas** (um por vez), **Lado a lado**, **Empilhados** e **Grade**. Nas divisoes todos ficam visiveis e cada um se ajusta ao seu tamanho; clique em um para dar foco (borda azul). A escolha fica salva no navegador.
- **Popup**: abre **uma nova sessao** da maquina da aba ativa em outra janela (util para outro monitor). A janela mostra so o terminal, fecha a sessao ao fechar e o navegador precisa liberar pop-ups para o site. Uma sessao ja aberta nao muda de janela.
- **Minimizar** volta para a lista de maquinas sem desconectar nada (o botao **Terminais (n)** no topo reabre); **Encerrar todos** fecha todas as sessoes.
- Fonte, tamanho e renderizador valem para todos os terminais. Com muitos terminais, prefira o renderizador padrao (DOM) ao WebGL.

### Terminal no navegador: fontes e ambiente

O botao **Terminal** abre um shell da maquina no navegador (xterm.js). Prompts como o Spaceship e ferramentas como o eza usam icones **Nerd Font / Font Awesome / Powerline**, e o navegador so enxerga as fontes instaladas **no computador de quem esta vendo o painel**. Por isso:

- O painel traz embutida a fonte de simbolos *Symbols Nerd Font Mono* (MIT) como reserva: os icones aparecem mesmo sem nenhuma fonte instalada.
- Para ficar identico ao seu terminal local, clique em **Fonte** na barra do terminal e informe o nome da sua Nerd Font (ex: `MesloLGS NF`, `JetBrainsMono Nerd Font`) e o tamanho. A escolha fica salva no navegador. Varias fontes comuns ja sao tentadas automaticamente.
- O shell abre em modo `xterm-256color` e `COLORTERM=truecolor`. Como um servico do systemd nao define quase nada, o agent completa o ambiente do shell quando falta: `TERM`, `HOME`, `USER`, `LOGNAME`, `SHELL`, `PATH`, locale UTF-8 (`C.UTF-8`; `en_US.UTF-8` no macOS), e abre a sessao no diretorio `HOME`. Valores ja definidos (por exemplo no `.env` do servico) nunca sao sobrescritos.
- **Copiar e colar**: com texto selecionado, `Ctrl+C` (ou `Cmd+C`) copia; sem selecao, `Ctrl+C` interrompe o comando, como num terminal comum. `Ctrl+Shift+C` sempre copia. `Ctrl+V`, `Ctrl+Shift+V` (e o menu do botao direito) colam. Selecione com o mouse (arrastar, duplo clique para palavra, triplo clique para a linha).
- **Renderizador**: o padrao desenha o texto com elementos do DOM. Se algo aparecer fora do lugar (texto repetido, glifos sobrepostos), abra **Fonte** e escolha **WebGL**, que desenha em canvas. A escolha fica salva no navegador.
- **Diagnostico**: o botao grava o que o terminal envia e recebe (com `\e` e `\b` visiveis) e mostra o buffer interno do xterm, para comparar o que ele guarda com o que aparece na tela. Util para investigar problemas de exibicao. Nao grave senhas.
- O shell e o definido por `--shell` (padrao `/bin/bash`). Para abrir direto no zsh com Oh My Zsh, use `--shell /usr/bin/zsh` (ou o caminho de `which zsh`) no servico do agent e o usuario que roda o servico deve ter o `~/.zshrc` configurado.

### Download do agent

A imagem do server leva o agent compilado para `linux/amd64`, `linux/arm64`, `linux/arm` (ARMv6 e superiores, como todos os Raspberry Pi), `darwin/amd64` e `darwin/arm64`, e os distribui em `GET /download/agent/<os>-<arch>`. O comando gerado pelo painel:

1. detecta a arquitetura com `uname -m`;
2. baixa o binario certo, autenticando com a **credencial da maquina** (enviada ao `curl` por stdin, entao nao aparece no `ps`);
3. instala em `/usr/local/bin/agent` (troca atomica, entao reexecutar o comando atualiza o agent) e, no modo systemd, cria e reinicia o servico.

**Versao e atualizacao.** O agent informa a sua versao ao conectar e o painel a mostra na tabela, com o aviso **desatualizado** quando ela difere da do server (a imagem Docker grava a mesma versao no server e nos agents que distribui). O botao **Atualizar agent** mostra um comando que baixa o agent atual e reinicia o servico reaproveitando a credencial ja salva em `/etc/rterminal-agent.env`, sem trocar a credencial. Atencao: o ambiente do shell (`TERM`, `HOME`, UTF-8) e montado pelo **agent**; um agent antigo abre shells sem `TERM`, e o zsh entao nao consegue mover o cursor (sintoma: caracteres digitados repetidos no prompt). Atualize os agents sempre que atualizar o server.

O endpoint tambem aceita o login do administrador (para baixar pelo navegador). Credenciais invalidas sao limitadas por IP, a lista de alvos e fixa (nada de caminhos livres) e cada download aparece na auditoria. Windows nao e suportado (o agent usa PTY).

A pasta de binarios vem de `--agents-dir` ou `RTERMINAL_AGENTS_DIR` (a imagem Docker ja a define). Ao rodar o server com `go run`, sem essa pasta, o download fica desativado e os comandos gerados assumem o `agent` ja instalado; para gerar os binarios na mao: `GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o agents/agent-linux-arm64 ./cmd/agent` (um arquivo `agent-<os>-<arch>` por plataforma).

### Deploy com Docker (Portainer + nginx-proxy)

`deploy/server/docker-compose.yml` sobe so o server com o painel, pensado para uma **stack do Portainer em Docker Swarm** atras de um **nginx-proxy** (com acme-companion/Let's Encrypt) que voce ja tem rodando. Ele nao publica porta no host: o proxy o alcanca pela rede overlay externa `proxy_net`, que a stack apenas referencia (`external: true`). O email do Let's Encrypt e o que ja estiver configurado no seu acme-companion.

1. A imagem do server fica no repositorio privado `thiagozs/go-rterminal` (veja [Imagem Docker](#imagem-docker)). No Portainer, cadastre as credenciais em **Registries > Add registry > DockerHub** para ele conseguir baixa-la.
2. **Stacks > Add stack > Web editor** e cole o `deploy/server/docker-compose.yml`. Em **Environment variables**, cadastre as de `deploy/server/.env.example`:
   - `DOMAIN`: dominio que ja aponta (DNS) para o servidor do proxy (a unica obrigatoria)
   - `TRUSTED_PROXIES`: faixa (*Subnet*) da rede `proxy_net` (**Networks** no Portainer; padrao `10.0.1.0/24`), para o server enxergar o IP real do cliente
   - `PLACEMENT_CONSTRAINT`: opcional; o server precisa rodar no mesmo no do nginx-proxy, que le o `docker.sock` local (padrao `node.role == manager`)
   - `SERVER_IMAGE`: opcional; por padrao `thiagozs/go-rterminal:server` (em producao, fixe uma tag datada)
3. Faca o deploy. O nginx-proxy detecta `VIRTUAL_HOST`/`LETSENCRYPT_HOST`, emite o certificado e passa a servir `https://DOMAIN/` (WebSockets incluidos, sem configuracao extra: o server envia pings a cada 20s, menos que o timeout padrao de 60s do nginx).
4. Pegue a senha temporaria em **Stacks > (stack) > server > Logs** (procure `PRIMEIRO ACESSO`) e entre para trocá-la.

O servico roda com uma unica replica, e as atualizacoes param o container antigo antes de subir o novo (`stop-first`), porque o banco e um SQLite em volume local. Ele fica no volume `rterminal-data` (`/data`), entao sobrevive a atualizacoes da stack, mas e local ao no: se o servico for movido para outro no, o banco nao vai junto. O container roda sem privilegios, com sistema de arquivos somente leitura e sem capabilities. As maquinas usam `wss://DOMAIN` nos comandos gerados.

### Imagem Docker

O alvo `server` do `Dockerfile` gera a imagem do server (Debian slim, usuario sem privilegios, banco em `/data`). Para construir e publicar no Docker Hub:

```bash
make image-server                         # constroi thiagozs/go-rterminal:server, :latest e :server-AAAAMMDD
docker login
make push-server                          # envia as tres tags
```

`IMAGE` e `TAG` podem ser trocados (`make image-server IMAGE=usuario/repo`).

A tag datada (`IMAGE_TAG`, por exemplo `server-20261127`) e gravada como versao no server e nos 5 agents embutidos (`-ldflags -X main.version`). Assim o painel compara a versao de cada agent com a do server e marca **desatualizado** quando diferem.

**Atualizar um ambiente ja no ar:**

1. no Portainer, atualize a stack com "Re-pull image" (a tag `latest` aponta para a build mais recente; para fixar uma versao, use `SERVER_IMAGE=thiagozs/go-rterminal:server-AAAAMMDD`);
2. no painel, nas maquinas marcadas como **desatualizado**, use **Atualizar agent** e rode o comando mostrado na maquina.

O banco em `/data` e preservado entre atualizacoes.

## Flags disponiveis

### server

- `--addr` endereco de escuta HTTP (ex: `:8080`)
- `--auth-token` token compartilhado (ou `RTERMINAL_TOKEN`); obrigatorio no modo legado, opcional com `--db` (so clientes CLI)
- `--db` arquivo SQLite; ativa o painel de administracao e as credenciais por maquina
- `--public-url` URL publica do server, usada nos comandos gerados (ex: `https://rt.exemplo.com`)
- `--admin-user` usuario do primeiro administrador (padrao `admin`); a senha inicial e temporaria, gerada e logada uma vez (ou vem de `RTERMINAL_ADMIN_PASSWORD`)
- `--agents-dir` pasta com os binarios `agent-<os>-<arch>` distribuidos em `/download/agent/` (padrao `$RTERMINAL_AGENTS_DIR`)
- `--trusted-proxies` CIDRs dos proxies reversos cujo `X-Forwarded-For` e confiavel (ex: `172.16.0.0/12`); sem isso, atras de um proxy todos os logins pareceriam vir do IP do proxy
- `--tls-cert` caminho do certificado TLS (opcional)
- `--tls-key` caminho da chave TLS (opcional)
- `--session-timeout` timeout para pareamento de sessao (padrao `15s`, `0` desativa)

### agent

- `--server-url` URL WS do server (ex: `ws://127.0.0.1:8080`)
- `--agent-id` ID do agent (ex: `agent-01`); com o painel, use o ID mostrado no comando gerado
- `--shell` shell para abrir no agente (ex: `/bin/bash`)
- `--auth-token` token de autenticacao (obrigatorio; ou `RTERMINAL_TOKEN`); com o painel, e a credencial da maquina
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

## Comportamento e padroes

![Painel com o backoff de reconexao do agent e os padroes de keepalive, pareamento e limites](docs/images/reliability.png)

- **Token obrigatorio**: nao existe token padrao; sem `--auth-token` nem `RTERMINAL_TOKEN` o processo nao sobe (o server com `--db` e a excecao: usa credenciais por maquina).
- **Agent unico por ID**: um segundo agent com o mesmo `agent-id` e recusado enquanto o primeiro responde a pings; uma conexao morta e substituida.
- **Shell so apos o pareamento**: o agent abre o shell apenas quando o server envia `paired`, entao sessoes que ninguem pareou nao gastam processo nem slot. Elas expiram em `--session-timeout`.
- **Limite de sessoes**: acima de `--max-sessions`, o client recebe `agent busy` (close 1013) na hora. Sessoes aguardando pareamento tambem tem um teto interno (64).
- **Keepalive**: o server envia ping a cada 20s para agents e sessoes pareadas e derruba quem ficar mudo por 40s. O agent tambem envia pings no canal de controle (`--keepalive-interval`).
- **Reconexao**: o agent reconecta com backoff exponencial e jitter completo (o atraso e sorteado entre `0` e o limite do grafico acima); o limite volta ao minimo ao conectar.
- **Resize**: o client reenvia o tamanho do terminal a cada `SIGWINCH` (Linux e macOS).
- **Limpeza**: ao fechar a sessao, o agent envia `SIGHUP` e depois `SIGKILL` a todos os processos da sessao do shell, inclusive jobs em background, e antes disso esvazia a saida pendente do PTY.
- **Shutdown gracioso**: `SIGINT`/`SIGTERM` encerram o server (`Shutdown` com 10s) e o agent (que encerra as sessoes ativas).

## Testes

```bash
go vet ./...
go test -race ./...
```

Os testes do `agent` rodam shells reais atras de um relay em `httptest` (saida completa, limpeza de processos filhos e pareamento).

## O que este projeto representa

Este projeto e uma base enxuta para estudar e testar:

- pareamento de clientes e agentes via WebSocket
- multiplexacao simples de sessoes
- comunicacao de terminal em modo raw com PTY, incluindo resize
- ciclo de vida de processos filhos em sessoes de PTY

Ele inclui autenticacao por token (ou por credencial de maquina, com o painel), auditoria basica no painel e suporte opcional a TLS, mas nao faz autorizacao por usuario ou por grupo, isolamento do shell ou criptografia propria. No modo legado o token e compartilhado por todos os agents e clientes; no painel, quem e administrador pode abrir um shell em qualquer maquina, entao proteja esse acesso (senha forte, HTTPS). Use `wss`/`https` fora de uma rede local confiavel. O agent concede ao usuario remoto os mesmos privilegios do processo que o executa; use uma conta dedicada e com privilegios minimos.

## Imagens

As imagens de `docs/images/` seguem o estilo 16 bits (paleta Sweetie-16, exatamente 16 cores) e sao geradas por `docs/images/gen.py` (precisa de Pillow):

```bash
python3 docs/images/gen.py
```

## Autor

Feito com ❤️ por **Thiago Zilli Sarmento**.

## License

Este projeto esta sob a licenca MIT. Veja o arquivo [LICENSE](LICENSE) para o texto completo.
