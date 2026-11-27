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
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
	"github.com/spf13/cobra"
	"github.com/thiagozs/go-rterminal/internal/auth"
	"github.com/thiagozs/go-rterminal/internal/backoff"
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
		}
		log.Printf("[agent] conexão perdida: %v", err)
		delay := retry.Next()
		log.Printf("[agent] reconectando em %s", delay)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
		}
	}
	log.Println("[agent] encerrando, aguardando sessões ativas")
	config.sessions.Wait()
	return nil
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
	ctrl, _, err := (&websocket.Dialer{TLSClientConfig: tlsConfig}).DialContext(ctx, controlURL, headers)
	if err != nil {
		return false, err
	}
	defer ctrl.Close()

	hostname, _ := os.Hostname()
	if err := ctrl.WriteJSON(protocol.Message{
		Type:      protocol.TypeRegisterAgent,
		AgentID:   config.agentID,
		SessionID: config.agentID, // Compatibilidade com servidores anteriores.
		Hostname:  hostname,
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
		Version:   version,
	}); err != nil {
		return false, fmt.Errorf("registrar agente: %w", err)
	}
	connected := true
	ctrl.SetReadLimit(1 << 20)
	log.Printf("[agent] conectado | agent=%s", config.agentID)

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
		if msg.Type != protocol.TypeStartSession || msg.SessionID == "" {
			log.Printf("[agent] mensagem de controle ignorada | type=%q", msg.Type)
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
	command.Env = shellEnvironment(os.Environ(), config.shellPath)
	command.Dir = startDirectory(command.Env)
	ptmx, err := pty.Start(command)
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
