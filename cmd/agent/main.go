package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
	"github.com/spf13/cobra"
	"github.com/thiagozs/go-rterminal/internal/auth"
	"github.com/thiagozs/go-rterminal/internal/backoff"
	"github.com/thiagozs/go-rterminal/internal/diag"
	"github.com/thiagozs/go-rterminal/internal/endpoint"
	"github.com/thiagozs/go-rterminal/internal/protocol"
	"github.com/thiagozs/go-rterminal/internal/tlsutil"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

const (
	drainTimeout = 2 * time.Second // max wait for PTY output after the shell exits
	killGrace    = 2 * time.Second // SIGHUP to SIGKILL escalation delay

	// maxPendingSessions bounds sessions that joined but are not paired yet.
	maxPendingSessions = 64
)

func main() {
	rootCmd := &cobra.Command{
		Use:           "agent",
		Version:       version,
		Short:         "Conecta um agente ao servidor do go-rterminal",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          runAgent,
	}
	rootCmd.Flags().String("server-url", "ws://127.0.0.1:8080", "URL WS do server")
	rootCmd.Flags().String("agent-id", "agent-01", "ID do agent")
	rootCmd.Flags().String("shell", "/bin/bash", "Shell para abrir no agente")
	rootCmd.Flags().String("auth-token", "", "Token de autenticacao (ou RTERMINAL_TOKEN)")
	rootCmd.Flags().Int("max-sessions", 8, "Maximo de sessoes de shell simultaneas (0 = sem limite)")
	rootCmd.Flags().String("tls-ca", "", "Caminho do CA para TLS (opcional)")
	rootCmd.Flags().Bool("tls-insecure", false, "Ignora verificacao TLS (nao recomendado)")
	rootCmd.Flags().Duration("backoff-min", 5*time.Second, "Backoff minimo para reconexao")
	rootCmd.Flags().Duration("backoff-max", 60*time.Second, "Backoff maximo para reconexao")
	rootCmd.Flags().Duration("keepalive-interval", 15*time.Second, "Intervalo de keepalive no controle")
	rootCmd.Flags().String("shell-user", "", "Abre os terminais e a transferencia de arquivos como este usuario (o agent roda como root e troca de usuario por sessao; o shell padrao passa a ser o dele)")
	rootCmd.Flags().Bool("no-files", false, "Desativa a transferencia de arquivos pelo painel")
	rootCmd.Flags().Bool("no-self-update", false, "Recusa o pedido de atualizacao automatica vindo do painel")
	rootCmd.Flags().String("events-file", "", "Arquivo do diario de eventos de conexao (padrao: /var/lib/rterminal-agent/events.jsonl como root, ou ~/.local/state/rterminal-agent/events.jsonl; \"off\" desativa)")
	cobra.CheckErr(rootCmd.Execute())
}

type agentConfig struct {
	serverURL         string
	agentID           string
	shellPath         string
	authToken         string
	tlsCA             string
	tlsInsecure       bool
	backoffMin        time.Duration
	backoffMax        time.Duration
	keepaliveInterval time.Duration
	maxSessions       int
	sessionSlots      semaphore // active shells; nil = unlimited
	pendingSlots      semaphore // sessions waiting for a peer; nil = unlimited
	sessions          *sync.WaitGroup
	journal           *diag.Journal // connection history; nil records nothing
	onConnected       func()        // called each time the control channel is registered
	noSelfUpdate      bool          // refuse update requests from the panel
	noFiles           bool          // refuse file transfer channels
	identity          *identity     // account terminals and files run as; nil = the agent's own
	supervised        bool          // run by systemd, which restarts the agent when it exits
	updater           *updater
}

// defaultEventsFile is where the connection journal lives when not configured.
func defaultEventsFile() string {
	if os.Geteuid() == 0 {
		return "/var/lib/rterminal-agent/events.jsonl"
	}
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "rterminal-agent", "events.jsonl")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".local", "state", "rterminal-agent", "events.jsonl")
	}
	return ""
}

func runAgent(cmd *cobra.Command, _ []string) error {
	config := agentConfig{}
	config.serverURL, _ = cmd.Flags().GetString("server-url")
	config.agentID, _ = cmd.Flags().GetString("agent-id")
	config.shellPath, _ = cmd.Flags().GetString("shell")
	flagToken, _ := cmd.Flags().GetString("auth-token")
	config.maxSessions, _ = cmd.Flags().GetInt("max-sessions")
	config.tlsCA, _ = cmd.Flags().GetString("tls-ca")
	config.tlsInsecure, _ = cmd.Flags().GetBool("tls-insecure")
	config.backoffMin, _ = cmd.Flags().GetDuration("backoff-min")
	config.backoffMax, _ = cmd.Flags().GetDuration("backoff-max")
	config.keepaliveInterval, _ = cmd.Flags().GetDuration("keepalive-interval")
	eventsFile, _ := cmd.Flags().GetString("events-file")
	config.noSelfUpdate, _ = cmd.Flags().GetBool("no-self-update")
	config.noFiles, _ = cmd.Flags().GetBool("no-files")
	shellUser, _ := cmd.Flags().GetString("shell-user")
	identity, err := resolveShellUser(shellUser)
	if err != nil {
		return err
	}
	config.identity = identity
	if identity != nil && !cmd.Flags().Changed("shell") && identity.shell != "" {
		config.shellPath = identity.shell // the account's own login shell
	}
	config.supervised = os.Getenv("INVOCATION_ID") != "" // set by systemd for the services it runs
	if err := config.validate(); err != nil {
		return err
	}
	token, err := auth.ResolveToken(flagToken)
	if err != nil {
		return err
	}
	config.authToken = token
	if config.maxSessions > 0 {
		config.sessionSlots = make(semaphore, config.maxSessions)
	}
	config.pendingSlots = make(semaphore, maxPendingSessions)

	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	config.sessions = &sync.WaitGroup{}

	switch eventsFile {
	case "off":
		eventsFile = ""
	case "":
		eventsFile = defaultEventsFile()
	}
	journal := diag.Open(eventsFile)
	config.journal = journal
	if exe, err := executablePath(); err == nil {
		config.updater = &updater{exe: exe, journal: journal, exit: os.Exit}
		config.updater.begin()
	}
	var noUpdate error = errors.New("não foi possível localizar o binário do agent")
	if config.updater != nil {
		noUpdate = config.updater.unavailable(config)
	}
	recordStart(journal, noUpdate, config.identity)
	go diag.WatchSuspend(ctx, journal)

	// link follows the connection so a reconnection can say how long the
	// agent was offline and how many attempts it took.
	var link struct {
		connectedAt time.Time
		offlineAt   time.Time
		attempts    int
		firstErr    string
	}
	config.onConnected = func() {
		detail := "conectado ao servidor"
		if !link.offlineAt.IsZero() {
			detail = fmt.Sprintf("reconectado depois de %s offline, em %d tentativa(s)", time.Since(link.offlineAt).Round(time.Second), link.attempts)
			if link.firstErr != "" {
				detail += "; causa: " + link.firstErr
			}
		}
		journal.Record("connected", detail)
		if config.updater != nil {
			config.updater.confirm()
		}
		link.connectedAt, link.offlineAt, link.attempts, link.firstErr = time.Now(), time.Time{}, 0, ""
	}

	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	retry := backoff.New(config.backoffMin, config.backoffMax, rng)
	for ctx.Err() == nil {
		log.Println("[agent] tentando conectar ao server")
		connected, err := runControl(ctx, config)
		if ctx.Err() != nil {
			break
		}
		if connected {
			retry.Reset()
			journal.Record("lost", fmt.Sprintf("conexão perdida depois de %s: %s | %s",
				time.Since(link.connectedAt).Round(time.Second), diag.Describe(err), diag.NetSnapshot()))
			link.offlineAt, link.attempts, link.firstErr = time.Now(), 0, ""
		} else {
			link.attempts++
			if link.offlineAt.IsZero() {
				link.offlineAt = time.Now()
			}
			// Only the first failure of an outage is recorded in full; the
			// reconnection event summarises the rest.
			if link.attempts == 1 {
				link.firstErr = diag.Describe(err)
				journal.Record("connect_failed", link.firstErr+" | "+diag.NetSnapshot())
			}
		}
		log.Printf("[agent] conexão perdida: %v", err)
		delay := retry.Next()
		log.Printf("[agent] reconectando em %s", delay)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
		}
	}
	journal.Record("agent_stop", "agent encerrado normalmente (sinal de parada)")
	log.Println("[agent] encerrando, aguardando sessões ativas")
	config.sessions.Wait()
	return nil
}

// recordStart notes that the agent started and whether the previous run ended
// cleanly, which tells a reboot or power loss apart from a normal restart.
func recordStart(j *diag.Journal, noUpdate error, id *identity) {
	detail := "agent " + version + " iniciado"
	if id != nil {
		detail += fmt.Sprintf("; terminais e arquivos abrem como o usuário %s (uid %d)", id.name, id.uid)
		log.Printf("[agent] terminais e arquivos abrem como o usuário %s (uid %d), shell %s", id.name, id.uid, id.shell)
	}
	if noUpdate != nil {
		detail += "; atualização automática indisponível: " + noUpdate.Error()
	}
	if up := diag.Uptime(); up > 0 {
		detail += fmt.Sprintf("; a máquina está ligada há %s", up.Round(time.Second))
	}
	if clean, known := j.PreviousRunEndedCleanly(); known && !clean {
		detail += "; a execução anterior não terminou normalmente (queda de energia, reinício forçado ou falha do agent)"
	}
	j.Record("agent_start", detail)
}

// capabilities lists what this agent offers the server.
func (c agentConfig) capabilities() []string {
	var caps []string
	if c.updater != nil && c.updater.unavailable(c) == nil {
		caps = append(caps, protocol.CapSelfUpdate)
	}
	if !c.noFiles && (c.identity == nil || fileIdentitySupported) {
		caps = append(caps, protocol.CapFiles)
	}
	return caps
}

func (c agentConfig) validate() error {
	if c.agentID == "" {
		return fmt.Errorf("agent-id não pode ser vazio")
	}
	if c.shellPath == "" {
		return fmt.Errorf("shell não pode ser vazio")
	}
	if c.backoffMin <= 0 || c.backoffMax <= 0 || c.backoffMin > c.backoffMax {
		return fmt.Errorf("backoff deve ser positivo e backoff-min não pode exceder backoff-max")
	}
	if c.maxSessions < 0 {
		return fmt.Errorf("max-sessions não pode ser negativo")
	}
	if c.keepaliveInterval < 0 {
		return fmt.Errorf("keepalive-interval não pode ser negativo")
	}
	return nil
}

func runControl(ctx context.Context, config agentConfig) (bool, error) {
	tlsConfig, err := tlsutil.ClientConfig(config.tlsCA, config.tlsInsecure)
	if err != nil {
		return false, err
	}
	controlURL, err := endpoint.URL(config.serverURL, "/agent", nil, true)
	if err != nil {
		return false, err
	}
	headers := http.Header{}
	auth.SetAuthorization(headers, config.authToken)
	ctrl, resp, err := (&websocket.Dialer{TLSClientConfig: tlsConfig}).DialContext(ctx, controlURL, headers)
	if err != nil {
		return false, diag.NewDialError(err, resp)
	}
	defer ctrl.Close()

	hostname, _ := os.Hostname()
	if err := ctrl.WriteJSON(protocol.Message{
		Type:         protocol.TypeRegisterAgent,
		AgentID:      config.agentID,
		SessionID:    config.agentID, // Compatibilidade com servidores anteriores.
		Hostname:     hostname,
		OS:           runtime.GOOS,
		Arch:         runtime.GOARCH,
		Version:      version,
		Capabilities: config.capabilities(),
	}); err != nil {
		return false, fmt.Errorf("registrar agente: %w", err)
	}
	connected := true
	ctrl.SetReadLimit(1 << 20)
	log.Printf("[agent] conectado | agent=%s", config.agentID)
	if config.onConnected != nil {
		config.onConnected()
	}
	sendDiagnostics(ctrl, config.journal)

	keepaliveDone := startKeepalive(ctrl, config.keepaliveInterval)
	defer close(keepaliveDone)
	// Unblock ReadJSON when the agent is asked to stop.
	go func() {
		select {
		case <-ctx.Done():
			_ = ctrl.Close()
		case <-keepaliveDone:
		}
	}()
	for {
		var msg protocol.Message
		if err := ctrl.ReadJSON(&msg); err != nil {
			return connected, err
		}
		if msg.Type == protocol.TypeUpdateAgent {
			if config.updater != nil {
				go config.updater.run(ctx, config, msg, func() { sendDiagnostics(ctrl, config.journal) })
			}
			continue
		}
		if msg.Type != protocol.TypeStartSession || msg.SessionID == "" {
			log.Printf("[agent] mensagem de controle ignorada | type=%q", msg.Type)
			continue
		}
		if msg.Kind == protocol.KindFiles {
			if config.noFiles {
				log.Printf("[agent] transferência de arquivos desativada, pedido recusado | session=%s", msg.SessionID)
				continue
			}
			if !config.pendingSlots.tryAcquire() {
				log.Printf("[agent] muitas sessões aguardando pareamento, canal de arquivos recusado | session=%s", msg.SessionID)
				continue
			}
			config.sessions.Add(1)
			go func(sessionID string) {
				defer config.sessions.Done()
				runFileSession(ctx, config, sessionID)
			}(msg.SessionID)
			continue
		}
		log.Printf("[agent] iniciando sessão | session=%s", msg.SessionID)
		if !config.pendingSlots.tryAcquire() {
			log.Printf("[agent] muitas sessões aguardando pareamento, sessão recusada | session=%s", msg.SessionID)
			continue
		}
		config.sessions.Add(1)
		go func(sessionID string) {
			defer config.sessions.Done()
			runShellSession(ctx, config, sessionID, tlsConfig)
		}(msg.SessionID)
	}
}

// sendDiagnostics hands the server the events recorded since the last
// delivery. Events that could not be sent stay queued for the next connection.
func sendDiagnostics(ctrl *websocket.Conn, j *diag.Journal) {
	for {
		batch := j.Take(diag.BatchSize)
		if len(batch) == 0 {
			return
		}
		_ = ctrl.SetWriteDeadline(time.Now().Add(10 * time.Second))
		err := ctrl.WriteJSON(protocol.Message{Type: protocol.TypeDiagnostics, Events: batch})
		_ = ctrl.SetWriteDeadline(time.Time{})
		if err != nil {
			j.Requeue(batch)
			return
		}
	}
}

func startKeepalive(conn *websocket.Conn, interval time.Duration) chan struct{} {
	done := make(chan struct{})
	if interval <= 0 {
		return done
	}
	_ = conn.SetReadDeadline(time.Now().Add(interval * 2))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(interval * 2))
	})
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				deadline := time.Now().Add(5 * time.Second)
				if err := conn.WriteControl(websocket.PingMessage, []byte("ping"), deadline); err != nil {
					_ = conn.Close()
					return
				}
			}
		}
	}()
	return done
}

// startShell starts command on a new PTY. With an identity the shell is
// started as that account (uid, gid and groups), and the PTY slave is handed
// to it so programs that open /dev/pts/N by name keep working.
func startShell(command *exec.Cmd, id *identity) (*os.File, error) {
	if id == nil {
		return pty.Start(command)
	}
	ptmx, tty, err := pty.Open()
	if err != nil {
		return nil, err
	}
	defer tty.Close()
	if err := os.Chown(tty.Name(), int(id.uid), -1); err != nil {
		ptmx.Close()
		return nil, fmt.Errorf("entregar o terminal ao usuário %s: %w", id.name, err)
	}
	command.Stdin, command.Stdout, command.Stderr = tty, tty, tty
	command.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true, Setctty: true,
		Credential: &syscall.Credential{Uid: id.uid, Gid: id.gid, Groups: id.groups},
	}
	if err := command.Start(); err != nil {
		ptmx.Close()
		return nil, err
	}
	return ptmx, nil
}

func runShellSession(ctx context.Context, config agentConfig, sessionID string, tlsConfig *tls.Config) {
	if err := shellSession(ctx, config, sessionID, tlsConfig); err != nil {
		log.Printf("[agent] sessão encerrada | session=%s | err=%v", sessionID, err)
		return
	}
	log.Printf("[agent] sessão encerrada | session=%s", sessionID)
}

// shellSession joins the session on the server, waits for the peer to pair and
// only then spawns the shell, so unpaired sessions cost no process or slot.
func shellSession(ctx context.Context, config agentConfig, sessionID string, tlsConfig *tls.Config) error {
	pendingReleased := false
	releasePending := func() {
		if !pendingReleased {
			pendingReleased = true
			config.pendingSlots.release()
		}
	}
	defer releasePending()

	sessionURL, err := endpoint.URL(config.serverURL, "/session", url.Values{"id": {sessionID}}, true)
	if err != nil {
		return err
	}
	headers := http.Header{}
	auth.SetAuthorization(headers, config.authToken)
	ws, _, err := (&websocket.Dialer{TLSClientConfig: tlsConfig}).DialContext(ctx, sessionURL, headers)
	if err != nil {
		return fmt.Errorf("conectar sessão: %w", err)
	}
	defer ws.Close()
	ws.SetReadLimit(1 << 20)

	if err := waitForPairing(ctx, ws); err != nil {
		return err
	}
	releasePending()
	if !config.sessionSlots.tryAcquire() {
		log.Printf("[agent] limite de sessões atingido, sessão recusada | session=%s max=%d", sessionID, config.maxSessions)
		_ = ws.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseTryAgainLater, "agent busy: max sessions reached"), time.Now().Add(time.Second))
		return nil
	}
	defer config.sessionSlots.release()

	command := exec.Command(config.shellPath)
	command.Env = sessionEnvironment(os.Environ(), config.shellPath, config.identity)
	command.Dir = startDirectory(command.Env)
	ptmx, err := startShell(command, config.identity)
	if err != nil {
		return fmt.Errorf("iniciar PTY: %w", err)
	}
	defer ptmx.Close()

	waitDone := make(chan error, 1)
	go func() { waitDone <- command.Wait() }()
	ptyDone := make(chan error, 1)
	wsDone := make(chan error, 1)
	go func() { ptyDone <- copyPTYToWebSocket(ptmx, ws) }()
	go func() { wsDone <- copyWebSocketToPTY(ws, ptmx) }()

	pgid := command.Process.Pid // pty.Start makes the shell a session leader: pid == sid.
	select {
	case waitErr := <-waitDone:
		// The shell exited: flush what is left in the PTY before closing, so
		// the last output is not lost.
		select {
		case <-ptyDone:
		case <-time.After(drainTimeout):
		}
		signalSession(pgid, syscall.SIGHUP)
		closeNormally(ws, "shell exited")
		return normalizeExitError(waitErr)
	case transferErr := <-ptyDone:
		waitErr := stopShell(pgid, waitDone)
		closeNormally(ws, "shell exited")
		if transferErr != nil && !isExpectedClose(transferErr) {
			return transferErr
		}
		return normalizeExitError(waitErr)
	case transferErr := <-wsDone:
		_ = ptmx.Close()
		waitErr := stopShell(pgid, waitDone)
		if transferErr != nil && !isExpectedClose(transferErr) {
			return transferErr
		}
		return normalizeExitError(waitErr)
	case <-ctx.Done():
		_ = ptmx.Close()
		waitErr := stopShell(pgid, waitDone)
		closeNormally(ws, "agent shutting down")
		return normalizeExitError(waitErr)
	}
}

// stopShell hangs up the shell's whole session and escalates to SIGKILL if
// it does not exit within killGrace, so no child process is left behind.
func stopShell(pgid int, waitDone <-chan error) error {
	signalSession(pgid, syscall.SIGHUP)
	select {
	case err := <-waitDone:
		signalSession(pgid, syscall.SIGKILL) // background jobs live in other process groups
		return err
	case <-time.After(killGrace):
		signalSession(pgid, syscall.SIGKILL)
		return <-waitDone
	}
}

func closeNormally(ws *websocket.Conn, reason string) {
	_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, reason), time.Now().Add(time.Second))
}

// waitForPairing blocks until the server reports the session as paired. The
// server closes unpaired sessions after its pairing timeout.
func waitForPairing(ctx context.Context, ws *websocket.Conn) error {
	stop := context.AfterFunc(ctx, func() { _ = ws.Close() })
	defer stop()
	for {
		var msg protocol.Message
		if err := ws.ReadJSON(&msg); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
				return errors.New("sessão expirou antes do pareamento")
			}
			return fmt.Errorf("aguardar pareamento: %w", err)
		}
		if msg.Type == protocol.TypePaired {
			return nil
		}
	}
}

func copyPTYToWebSocket(reader io.Reader, ws *websocket.Conn) error {
	buffer := make([]byte, 4096)
	for {
		n, err := reader.Read(buffer)
		if n > 0 {
			if writeErr := ws.WriteMessage(websocket.BinaryMessage, buffer[:n]); writeErr != nil {
				return writeErr
			}
		}
		if err != nil {
			return err
		}
	}
}

func copyWebSocketToPTY(ws *websocket.Conn, ptmx *os.File) error {
	for {
		messageType, data, err := ws.ReadMessage()
		if err != nil {
			return err
		}
		switch messageType {
		case websocket.BinaryMessage:
			if _, err := ptmx.Write(data); err != nil {
				return err
			}
		case websocket.TextMessage:
			var msg protocol.Message
			if json.Unmarshal(data, &msg) == nil && msg.Type == protocol.TypeResize && msg.Rows > 0 && msg.Cols > 0 {
				if err := pty.Setsize(ptmx, &pty.Winsize{Rows: msg.Rows, Cols: msg.Cols}); err != nil {
					return fmt.Errorf("redimensionar PTY: %w", err)
				}
			}
		}
	}
}

func normalizeExitError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := err.(*exec.ExitError); ok {
		return nil
	}
	return err
}

func isExpectedClose(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, syscall.EIO) ||
		websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway)
}

// semaphore is a non-blocking counting limiter; a nil semaphore is unlimited.
type semaphore chan struct{}

// tryAcquire reserves a slot without blocking and reports whether it got one.
func (s semaphore) tryAcquire() bool {
	if s == nil {
		return true
	}
	select {
	case s <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s semaphore) release() {
	if s != nil {
		<-s
	}
}
