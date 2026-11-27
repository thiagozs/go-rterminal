# Roadmap

Contexto: homelab, mais de uma pessoa usando, painel exposto na internet com shell em todas as máquinas.

Legenda: `[x]` feito, `[ ]` pendente.

## Já feito

- [x] Painel web de administração (máquinas, grupos, credenciais, download do agent multi-arch)
- [x] Vários terminais na mesma tela (abas, split e popup)
- [x] Papéis `admin` / `operador` e acesso restrito por grupo
- [x] Gravação de sessões e reprodução na auditoria (parar, pausar, avançar, voltar, copiar)
- [x] Indicador "● gravando" no terminal
- [x] Autenticação em dois fatores
  - [x] App autenticador (TOTP) com QR code
  - [x] Código (PIN) por e-mail
  - [x] Códigos de recuperação
  - [x] Política `--require-2fa` (`none`, `admins`, `all`) e comando `reset-2fa`
- [x] Envio de e-mail por Resend ou SMTP (Mailpit para testes)

## Próximas features (em ordem de valor)

- [x] **1. Acompanhar sessão ao vivo (modo espectador)**
  - [x] Admin entra em modo somente leitura numa sessão em andamento
  - [x] Transmitir a saída em tempo real (reaproveitar o `Tap` da gravação)
  - [x] Lista de sessões ativas no painel
  - [x] Auditoria de quem assistiu (`watch_start` / `watch_end`)
  - [ ] Aviso "● observado" para quem está no terminal (decidir se queremos)
- [x] **2. Transferência de arquivos**
  - [x] Enviar arquivo pelo painel (botão e arrastar e soltar)
  - [x] Baixar arquivo da máquina
  - [x] Navegar pastas e criar pasta
  - [x] Registrar na auditoria (bytes reais) e limite de tamanho
  - [ ] Enviar pastas inteiras (arrastar uma pasta)
  - [ ] Excluir e renomear pelo painel
  - [ ] Baixar vários arquivos / pasta como .zip
  - [ ] Download em streaming para disco (arquivos muito grandes)
- [ ] **3. Alertas (e-mail, Telegram ou webhook)**
  - [ ] Agent que caiu
  - [ ] Excesso de logins falhos
  - [ ] Terminal aberto fora do horário
- [ ] **4. Comandos salvos e execução em várias máquinas**
  - [ ] Biblioteca de comandos
  - [ ] Executar em um grupo inteiro
  - [ ] Resultado por máquina
- [x] **5. Atualização do agent pelo painel**
  - [x] O agent baixa a nova versão e reinicia sozinho, em um clique
  - [x] Checksum SHA-256 e teste do binário novo (`--version`) antes de trocar
  - [x] Rollback automático se a versão nova não reconectar em 2 min
  - [x] Versão única por build (data + hora)
  - [ ] Atualizar um grupo inteiro de máquinas de uma vez
  - [ ] Suporte a macOS (launchd)
- [ ] **6. Expiração e aprovação de acesso**
  - [ ] Acesso temporário a um grupo (ex.: 2 horas)
  - [ ] Pedido de acesso com aprovação de um administrador
- [ ] **7. Backup e exportação**
  - [ ] Backup do banco pelo painel
  - [ ] Exportação da auditoria em CSV

## Diagnóstico

- [x] Histórico de conexão por máquina (agent anota suspensão, queda de rede, reinício; servidor anota online/offline)
  - [x] Diário local do agent (`--events-file`) enviado ao servidor ao reconectar
  - [x] Botão "Histórico" no painel
  - [ ] Alerta quando uma máquina fica offline (liga com o item "Alertas")
  - [ ] Linha do tempo agregada (percentual online por dia)

## Agent

- [x] Agent como root abrindo terminal e arquivos como outro usuário (`--shell-user`)
  - [x] Campo "Usuário do terminal" no cadastro e na edição da máquina
  - [x] Comando de migração para serviços que usam `User=`
  - [ ] Escolher o usuário por sessão (lista de contas da máquina)
  - [ ] Suporte a arquivos como outro usuário no macOS

## Melhorias conhecidas (limitações atuais)

- [ ] Derrubar terminais abertos quando a permissão do usuário muda
- [ ] Gravar também as sessões do cliente de linha de comando
- [ ] "Lembrar este dispositivo" no 2FA
- [ ] Separar a chave `2fa.key` do volume do banco
- [ ] Testar o Resend com uma chave real

## Depois (só compensa com mais gente)

- [ ] SSO (OIDC, Google, GitHub)
- [ ] Rede entre as máquinas (produto separado)
