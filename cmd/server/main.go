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
	"github.com/thiagozs/go-rterminal/internal/protocol"
	"github.com/thiagozs/go-rterminal/internal/recording"
	"github.com/thiagozs/go-rterminal/internal/relay"
	"github.com/thiagozs/go-rterminal/internal/store"
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
	recordSessions, _ := cmd.Flags().GetBool("recordings")
	recordingsDir, _ := cmd.Flags().GetString("recordings-dir")
	recordingsMaxMB, _ := cmd.Flags().GetInt("recordings-max-mb")
	retentionDays, _ := cmd.Flags().GetInt("recordings-retention-days")
	if recordingsMaxMB < 1 || retentionDays < 0 {
		return fmt.Errorf("recordings-max-mb deve ser >= 1 e recordings-retention-days >= 0")
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
		relayConfig.OnAgentConnected = func(id string, info protocol.Message) {
			if err := st.TouchMachine(id, info.Hostname, info.OS, info.Arch, info.Version); err != nil {
				log.Printf("[server] falha ao registrar conexão | agent=%s err=%v", id, err)
			}
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
		panel := admin.New(admin.Config{Store: st, Relay: relayServer, PublicURL: publicURL, TrustedProxies: trustedProxies, AgentsDir: agentsDir, Version: version, Recordings: recordings})
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
