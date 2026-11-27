package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/thiagozs/go-rterminal/internal/admin"
	"github.com/thiagozs/go-rterminal/internal/auth"
	"github.com/thiagozs/go-rterminal/internal/mailer"
	"github.com/thiagozs/go-rterminal/internal/protocol"
	"github.com/thiagozs/go-rterminal/internal/recording"
	"github.com/thiagozs/go-rterminal/internal/relay"
	"github.com/thiagozs/go-rterminal/internal/store"
	"github.com/thiagozs/go-rterminal/internal/twofactor"
)

const envAdminPassword = "RTERMINAL_ADMIN_PASSWORD"

// version is set at build time (-ldflags "-X main.version=..."); agents built
// from the same image report the same string, so the panel can spot stale ones.
var version = "dev"

func main() {
	rootCmd := &cobra.Command{
		Use:           "server",
		Short:         "Inicia o servidor do go-rterminal",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          runServer,
	}
	rootCmd.Flags().String("addr", ":8080", "Endereco de escuta HTTP")
	rootCmd.Flags().String("auth-token", "", "Token compartilhado (ou RTERMINAL_TOKEN). Modo legado: autentica agentes e clientes. Com --db: so clientes CLI")
	rootCmd.Flags().String("tls-cert", "", "Caminho do certificado TLS (opcional)")
	rootCmd.Flags().String("tls-key", "", "Caminho da chave TLS (opcional)")
	rootCmd.Flags().Duration("session-timeout", 15*time.Second, "Timeout para pareamento de sessao")
	rootCmd.Flags().String("db", "", "Arquivo SQLite. Ativa o painel de administracao e as credenciais por maquina")
	rootCmd.Flags().String("public-url", "", "URL publica do server (ex: https://rt.exemplo.com), usada nos comandos gerados")
	rootCmd.Flags().String("admin-user", "admin", "Usuario do primeiro administrador (criado quando o banco nao tem nenhum)")
	rootCmd.Flags().String("agents-dir", os.Getenv("RTERMINAL_AGENTS_DIR"), "Pasta com os binarios agent-<os>-<arch> que o server distribui em /download/agent/ (padrao: $RTERMINAL_AGENTS_DIR)")
	rootCmd.Flags().Bool("recordings", true, "Grava as sessoes do terminal web para reproduzir na auditoria (so com --db)")
	rootCmd.Flags().String("recordings-dir", "", "Pasta das gravacoes (padrao: a pasta \"recordings\" ao lado do --db)")
	rootCmd.Flags().Int("recordings-max-mb", 50, "Tamanho maximo de uma gravacao; a sessao e cortada com um aviso ao passar")
	rootCmd.Flags().Int("recordings-retention-days", 90, "Apaga gravacoes mais antigas que isso (0 = guarda para sempre)")
	rootCmd.Flags().Bool("files", true, "Permite enviar e baixar arquivos das maquinas pelo painel (so com --db)")
	rootCmd.Flags().Int("max-transfer-mb", 512, "Tamanho maximo de um arquivo enviado ou baixado pelo painel")
	rootCmd.Flags().String("require-2fa", "none", "Exige verificacao em duas etapas: none, admins ou all (quem nao tem fica so na tela de configuracao)")
	rootCmd.Flags().String("2fa-issuer", "go-rterminal", "Nome que aparece no app autenticador")
	rootCmd.Flags().String("resend-api-key", "", "Chave da API do Resend para o PIN por e-mail (prefira RTERMINAL_RESEND_API_KEY, que nao aparece no ps)")
	rootCmd.Flags().String("mail-from", "", "Remetente dos e-mails, para Resend ou SMTP (ex: \"Painel <painel@exemplo.com>\"; no Resend o dominio precisa estar verificado)")
	rootCmd.Flags().String("smtp-host", "", "Servidor SMTP para o PIN por e-mail, alternativa ao Resend (sem nenhum dos dois, so o app autenticador fica disponivel)")
	rootCmd.Flags().Int("smtp-port", 587, "Porta do SMTP")
	rootCmd.Flags().String("smtp-user", "", "Usuario do SMTP (vazio = sem autenticacao)")
	rootCmd.Flags().String("smtp-password", "", "Senha do SMTP (prefira RTERMINAL_SMTP_PASSWORD, que nao aparece no ps)")
	rootCmd.Flags().String("smtp-from", "", "Nome antigo de --mail-from")
	_ = rootCmd.Flags().MarkHidden("smtp-from")
	rootCmd.Flags().String("smtp-security", "starttls", "Protecao da conexao SMTP: starttls (587), tls (465) ou none (so relay local)")
	rootCmd.Flags().StringSlice("trusted-proxies", nil, "CIDRs de proxies confiaveis cujo X-Forwarded-For vale (ex: 172.16.0.0/12 para o nginx-proxy em Docker)")

	resetCmd := &cobra.Command{
		Use:   "reset-password",
		Short: "Gera uma senha temporaria para um administrador (recuperacao de acesso)",
		RunE:  runResetPassword,
	}
	resetCmd.Flags().String("db", "", "Arquivo SQLite do server")
	resetCmd.Flags().String("user", "admin", "Usuario do administrador")
	_ = resetCmd.MarkFlagRequired("db")
	rootCmd.AddCommand(resetCmd)

	reset2FACmd := &cobra.Command{
		Use:   "reset-2fa",
		Short: "Remove a verificacao em duas etapas de um usuario (recuperacao de acesso)",
		RunE:  runReset2FA,
	}
	reset2FACmd.Flags().String("db", "", "Arquivo SQLite do server")
	reset2FACmd.Flags().String("user", "admin", "Usuario que perdeu o segundo fator")
	_ = reset2FACmd.MarkFlagRequired("db")
	rootCmd.AddCommand(reset2FACmd)
	cobra.CheckErr(rootCmd.Execute())
}

func runServer(cmd *cobra.Command, _ []string) error {
	addr, _ := cmd.Flags().GetString("addr")
	flagToken, _ := cmd.Flags().GetString("auth-token")
	tlsCert, _ := cmd.Flags().GetString("tls-cert")
	tlsKey, _ := cmd.Flags().GetString("tls-key")
	sessionTimeout, _ := cmd.Flags().GetDuration("session-timeout")
	dbPath, _ := cmd.Flags().GetString("db")
	publicURL, _ := cmd.Flags().GetString("public-url")
	adminUser, _ := cmd.Flags().GetString("admin-user")
	agentsDir, _ := cmd.Flags().GetString("agents-dir")
	proxyCIDRs, _ := cmd.Flags().GetStringSlice("trusted-proxies")
	fileTransfer, _ := cmd.Flags().GetBool("files")
	maxTransferMB, _ := cmd.Flags().GetInt("max-transfer-mb")
	if maxTransferMB < 1 {
		return fmt.Errorf("max-transfer-mb deve ser >= 1")
	}
	recordSessions, _ := cmd.Flags().GetBool("recordings")
	recordingsDir, _ := cmd.Flags().GetString("recordings-dir")
	recordingsMaxMB, _ := cmd.Flags().GetInt("recordings-max-mb")
	retentionDays, _ := cmd.Flags().GetInt("recordings-retention-days")
	if recordingsMaxMB < 1 || retentionDays < 0 {
		return fmt.Errorf("recordings-max-mb deve ser >= 1 e recordings-retention-days >= 0")
	}
	require2FA, _ := cmd.Flags().GetString("require-2fa")
	issuer, _ := cmd.Flags().GetString("2fa-issuer")
	if require2FA != "none" && require2FA != "admins" && require2FA != "all" {
		return fmt.Errorf("require-2fa deve ser none, admins ou all")
	}
	mailSender, mailProvider, err := mailFromFlags(cmd)
	if err != nil {
		return err
	}
	trustedProxies, err := parseCIDRs(proxyCIDRs)
	if err != nil {
		return err
	}
	if sessionTimeout < 0 {
		return fmt.Errorf("session-timeout não pode ser negativo")
	}
	if (tlsCert == "") != (tlsKey == "") {
		return fmt.Errorf("tls-cert e tls-key devem ser informados juntos")
	}
	if publicURL != "" {
		u, err := url.Parse(publicURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("public-url inválida: use http(s)://host[:porta]")
		}
	}

	// The shared token is mandatory in legacy mode; with --db it is optional and
	// only lets CLI clients open sessions.
	sharedToken := ""
	if dbPath == "" {
		token, err := auth.ResolveToken(flagToken)
		if err != nil {
			return err
		}
		sharedToken = token
	} else if flagToken != "" || os.Getenv(auth.EnvToken) != "" {
		token, err := auth.ResolveToken(flagToken)
		if err != nil {
			return err
		}
		sharedToken = token
	}

	relayConfig := relay.Config{AuthToken: sharedToken, SessionTimeout: sessionTimeout}
	var handler http.Handler
	if dbPath == "" {
		handler = relay.New(relayConfig).Handler()
	} else {
		st, err := store.Open(dbPath)
		if err != nil {
			return err
		}
		defer st.Close()
		if err := bootstrapAdmin(st, adminUser); err != nil {
			return err
		}
		relayConfig.AgentAuth = func(token string) (string, bool) {
			machine, err := st.MachineByToken(token)
			if err != nil {
				return "", false
			}
			return machine.ID, true
		}
		recordEvent := func(id, source, kind, detail string) {
			if err := st.AddMachineEvents(id, source, []store.MachineEvent{{At: time.Now(), Kind: kind, Detail: detail}}); err != nil {
				log.Printf("[server] falha ao registrar evento | agent=%s err=%v", id, err)
			}
		}
		relayConfig.OnAgentConnected = func(id string, info protocol.Message) {
			if err := st.TouchMachine(id, info.Hostname, info.OS, info.Arch, info.Version); err != nil {
				log.Printf("[server] falha ao registrar conexão | agent=%s err=%v", id, err)
			}
			recordEvent(id, "server", "online", "agent conectou (versão "+info.Version+")")
		}
		relayConfig.OnAgentDisconnected = func(id, reason string) {
			recordEvent(id, "server", "offline", reason)
		}
		relayConfig.OnAgentEvents = func(id string, events []protocol.Event) {
			list := make([]store.MachineEvent, 0, len(events))
			for _, e := range events {
				list = append(list, store.MachineEvent{At: e.At, Kind: e.Kind, Detail: e.Detail})
			}
			if err := st.AddMachineEvents(id, "agent", list); err != nil {
				log.Printf("[server] falha ao guardar eventos do agent | agent=%s err=%v", id, err)
			}
		}
		sealer, err := twofactor.LoadSealer(filepath.Join(filepath.Dir(dbPath), "2fa.key"))
		if err != nil {
			return err
		}
		if require2FA != "none" {
			log.Printf("[server] verificação em duas etapas obrigatória para: %s", map[string]string{"admins": "administradores", "all": "todos os usuários"}[require2FA])
		}
		if mailSender != nil {
			log.Printf("[server] PIN por e-mail disponível (%s)", mailProvider)
		}
		var recordings *recording.Manager
		if recordSessions {
			if recordingsDir == "" {
				recordingsDir = filepath.Join(filepath.Dir(dbPath), "recordings")
			}
			recordings, err = recording.NewManager(recordingsDir, st, int64(recordingsMaxMB)<<20, nil)
			if err != nil {
				return err
			}
			recordings.Recover()
			done := make(chan struct{})
			defer close(done)
			go purgeRecordings(recordings, retentionDays, done)
			log.Printf("[server] gravação de sessões ativa em %s (limite %d MB por sessão, retenção %s)", recordingsDir, recordingsMaxMB, retentionText(retentionDays))
		} else {
			log.Printf("[server] gravação de sessões desativada")
		}
		relayServer := relay.New(relayConfig)
		panel := admin.New(admin.Config{Store: st, Relay: relayServer, PublicURL: publicURL, TrustedProxies: trustedProxies, AgentsDir: agentsDir, Version: version, Recordings: recordings, DisableFiles: !fileTransfer, MaxTransferBytes: int64(maxTransferMB) << 20,
			Sealer: sealer, Mailer: mailSender, Require2FA: require2FA, Issuer: issuer})
		logAgentDownloads(agentsDir)

		mux := http.NewServeMux()
		relayHandler := relayServer.Handler()
		for _, path := range []string{"/agent", "/client", "/session", "/healthz"} {
			mux.Handle(path, relayHandler)
		}
		mux.Handle("/", panel.Handler())
		handler = mux
		if tlsCert == "" && (publicURL == "" || !strings.HasPrefix(publicURL, "https")) {
			log.Printf("[server] AVISO: o painel de administração está sem TLS; use --tls-cert/--tls-key ou um proxy HTTPS com --public-url https://...")
		}
	}

	server := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	serveErr := make(chan error, 1)
	go func() {
		log.Printf("[server] escutando em %s", addr)
		if dbPath != "" {
			log.Printf("[server] painel de administração em %s/", displayURL(addr, tlsCert != "", publicURL))
		}
		if tlsCert != "" {
			serveErr <- server.ListenAndServeTLS(tlsCert, tlsKey)
			return
		}
		serveErr <- server.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		return fmt.Errorf("iniciar server: %w", err)
	case <-ctx.Done():
	}
	log.Println("[server] encerrando")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("encerrar server: %w", err)
	}
	return nil
}

func retentionText(days int) string {
	if days == 0 {
		return "indefinida"
	}
	return fmt.Sprintf("%d dias", days)
}

// purgeRecordings applies the retention period now and every six hours.
func purgeRecordings(m *recording.Manager, days int, done <-chan struct{}) {
	if days == 0 {
		return
	}
	run := func() {
		n, err := m.Purge(time.Now().AddDate(0, 0, -days))
		if err != nil {
			log.Printf("[server] falha ao aplicar retenção das gravações: %v", err)
		} else if n > 0 {
			log.Printf("[server] %d gravações antigas apagadas (retenção de %d dias)", n, days)
		}
	}
	run()
	ticker := time.NewTicker(6 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			run()
		case <-done:
			return
		}
	}
}

func displayURL(addr string, tls bool, publicURL string) string {
	if publicURL != "" {
		return publicURL
	}
	scheme := "http"
	if tls {
		scheme = "https"
	}
	if len(addr) > 0 && addr[0] == ':' {
		addr = "localhost" + addr
	}
	return scheme + "://" + addr
}

// logAgentDownloads reports which agent binaries the server will hand out.
func logAgentDownloads(dir string) {
	if dir == "" {
		log.Printf("[server] download do agent desativado (sem --agents-dir); os comandos gerados assumem o agent ja instalado")
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		log.Printf("[server] AVISO: --agents-dir %q ilegivel (%v); download do agent desativado", dir, err)
		return
	}
	var names []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "agent-") {
			names = append(names, strings.TrimPrefix(entry.Name(), "agent-"))
		}
	}
	log.Printf("[server] download do agent habilitado | %d binarios: %s", len(names), strings.Join(names, " "))
}

func parseCIDRs(values []string) ([]*net.IPNet, error) {
	var nets []*net.IPNet
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		_, network, err := net.ParseCIDR(value)
		if err != nil {
			return nil, fmt.Errorf("trusted-proxies: CIDR inválido %q", value)
		}
		nets = append(nets, network)
	}
	return nets, nil
}

// temporaryPassword returns a random password for first access or recovery.
func temporaryPassword() (string, error) {
	buf := make([]byte, 18)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// bootstrapAdmin creates the first administrator when the database has none.
// By default a temporary password is generated and logged once, and the admin
// must change it at the first login. RTERMINAL_ADMIN_PASSWORD overrides that
// with a password the operator chose (no forced change).
func bootstrapAdmin(st *store.Store, username string) error {
	count, err := st.CountAdmins()
	if err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	password := os.Getenv(envAdminPassword)
	temporary := password == ""
	if temporary {
		if password, err = temporaryPassword(); err != nil {
			return err
		}
	}
	if _, err := st.CreateAdmin(username, password, temporary); err != nil {
		if errors.Is(err, store.ErrInvalid) {
			return fmt.Errorf("admin inválido: usuário vazio ou senha com menos de %d caracteres (%s)", store.MinPasswordLength, envAdminPassword)
		}
		return err
	}
	if temporary {
		log.Printf("[server] ============================================================")
		log.Printf("[server] PRIMEIRO ACESSO: administrador criado com senha TEMPORARIA")
		log.Printf("[server]   usuario: %s", username)
		log.Printf("[server]   senha:   %s", password)
		log.Printf("[server]   Ela aparece só desta vez e deve ser trocada no primeiro login.")
		log.Printf("[server] ============================================================")
	} else {
		log.Printf("[server] primeiro administrador criado | usuario=%s (senha de %s)", username, envAdminPassword)
	}
	return nil
}

// mailFromFlags builds the e-mail sender and describes it for the log. It
// returns a nil sender (and no error) when no provider was configured: a typed
// nil would not be a nil interface, hence the helper.
func mailFromFlags(cmd *cobra.Command) (admin.Mailer, string, error) {
	host, _ := cmd.Flags().GetString("smtp-host")
	apiKey, _ := cmd.Flags().GetString("resend-api-key")
	if apiKey == "" {
		apiKey = os.Getenv("RTERMINAL_RESEND_API_KEY")
	}
	from, _ := cmd.Flags().GetString("mail-from")
	if from == "" {
		from, _ = cmd.Flags().GetString("smtp-from") // older name of the same setting
	}
	switch {
	case apiKey != "" && host != "":
		return nil, "", fmt.Errorf("escolha um provedor de e-mail: Resend (--resend-api-key) ou SMTP (--smtp-host), não os dois")
	case apiKey != "":
		m, err := mailer.NewResend(mailer.ResendConfig{APIKey: apiKey, From: from})
		if err != nil {
			return nil, "", err
		}
		return m, "Resend", nil
	case host != "":
		port, _ := cmd.Flags().GetInt("smtp-port")
		user, _ := cmd.Flags().GetString("smtp-user")
		password, _ := cmd.Flags().GetString("smtp-password")
		if password == "" {
			password = os.Getenv("RTERMINAL_SMTP_PASSWORD")
		}
		security, _ := cmd.Flags().GetString("smtp-security")
		m, err := mailer.New(mailer.Config{Host: host, Port: port, Username: user, Password: password, From: from, Security: security})
		if err != nil {
			return nil, "", err
		}
		return m, fmt.Sprintf("SMTP %s:%d", host, port), nil
	}
	return nil, "", nil
}

func runReset2FA(cmd *cobra.Command, _ []string) error {
	dbPath, _ := cmd.Flags().GetString("db")
	user, _ := cmd.Flags().GetString("user")
	if _, err := os.Stat(dbPath); err != nil {
		return fmt.Errorf("banco não encontrado: %s", dbPath)
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	account, err := st.UserByName(user)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("usuário %q não existe", user)
		}
		return err
	}
	if err := st.ClearTwoFactor(account.ID); err != nil {
		return err
	}
	fmt.Printf("Verificação em duas etapas de %q removida e sessões encerradas. Ela entra só com a senha e pode configurar de novo.\n", user)
	return nil
}

func runResetPassword(cmd *cobra.Command, _ []string) error {
	dbPath, _ := cmd.Flags().GetString("db")
	user, _ := cmd.Flags().GetString("user")
	if _, err := os.Stat(dbPath); err != nil {
		return fmt.Errorf("banco não encontrado: %s", dbPath)
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	password, err := temporaryPassword()
	if err != nil {
		return err
	}
	if err := st.ResetAdminPassword(user, password); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("administrador %q não existe", user)
		}
		return err
	}
	fmt.Printf("Senha temporária de %q: %s\nEla deve ser trocada no próximo login; as sessões abertas foram encerradas.\n", user, password)
	return nil
}
