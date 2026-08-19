package main

import (
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
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
	"github.com/spf13/cobra"
	"github.com/thiagozs/go-rterminal/internal/backoff"
	"github.com/thiagozs/go-rterminal/internal/endpoint"
	"github.com/thiagozs/go-rterminal/internal/protocol"
	"github.com/thiagozs/go-rterminal/internal/tlsutil"
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
	rootCmd.Flags().String("auth-token", "change-me", "Token de autenticacao")
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
}

func runAgent(cmd *cobra.Command, _ []string) error {
	config := agentConfig{}
	config.serverURL, _ = cmd.Flags().GetString("server-url")
	config.agentID, _ = cmd.Flags().GetString("agent-id")
	config.shellPath, _ = cmd.Flags().GetString("shell")
	config.authToken, _ = cmd.Flags().GetString("auth-token")
	config.tlsCA, _ = cmd.Flags().GetString("tls-ca")
	config.tlsInsecure, _ = cmd.Flags().GetBool("tls-insecure")
	config.backoffMin, _ = cmd.Flags().GetDuration("backoff-min")
	config.backoffMax, _ = cmd.Flags().GetDuration("backoff-max")
	config.keepaliveInterval, _ = cmd.Flags().GetDuration("keepalive-interval")
	if err := config.validate(); err != nil {
		return err
	}

	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	retry := backoff.New(config.backoffMin, config.backoffMax, rng)
	for {
		log.Println("[agent] tentando conectar ao server")
		connected, err := runControl(config)
		if connected {
			retry.Reset()
		}
		log.Printf("[agent] conexão perdida: %v", err)
		delay := retry.Next()
		log.Printf("[agent] reconectando em %s", delay)
		time.Sleep(delay)
	}
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
	if c.keepaliveInterval < 0 {
		return fmt.Errorf("keepalive-interval não pode ser negativo")
	}
	return nil
}

func runControl(config agentConfig) (bool, error) {
	tlsConfig, err := tlsutil.ClientConfig(config.tlsCA, config.tlsInsecure)
	if err != nil {
		return false, err
	}
	controlURL, err := endpoint.URL(config.serverURL, "/agent", nil, true)
	if err != nil {
		return false, err
	}
	headers := http.Header{}
	setAuthorization(headers, config.authToken)
	ctrl, _, err := (&websocket.Dialer{TLSClientConfig: tlsConfig}).Dial(controlURL, headers)
	if err != nil {
		return false, err
	}
	defer ctrl.Close()

	if err := ctrl.WriteJSON(protocol.Message{
		Type:      protocol.TypeRegisterAgent,
		AgentID:   config.agentID,
		SessionID: config.agentID, // Compatibilidade com servidores anteriores.
	}); err != nil {
		return false, fmt.Errorf("registrar agente: %w", err)
	}
	connected := true
	ctrl.SetReadLimit(1 << 20)
	log.Printf("[agent] conectado | agent=%s", config.agentID)

	keepaliveDone := startKeepalive(ctrl, config.keepaliveInterval)
	defer close(keepaliveDone)
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
		go runShellSession(config.serverURL, config.shellPath, msg.SessionID, config.authToken, tlsConfig)
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

func runShellSession(serverURL, shellPath, sessionID, authToken string, tlsConfig *tls.Config) {
	if err := shellSession(serverURL, shellPath, sessionID, authToken, tlsConfig); err != nil {
		log.Printf("[agent] sessão encerrada | session=%s | err=%v", sessionID, err)
		return
	}
	log.Printf("[agent] sessão encerrada | session=%s", sessionID)
}

func shellSession(serverURL, shellPath, sessionID, authToken string, tlsConfig *tls.Config) error {
	sessionURL, err := endpoint.URL(serverURL, "/session", url.Values{"id": {sessionID}}, true)
	if err != nil {
		return err
	}
	headers := http.Header{}
	setAuthorization(headers, authToken)
	ws, _, err := (&websocket.Dialer{TLSClientConfig: tlsConfig}).Dial(sessionURL, headers)
	if err != nil {
		return fmt.Errorf("conectar sessão: %w", err)
	}
	defer ws.Close()
	ws.SetReadLimit(1 << 20)

	command := exec.Command(shellPath)
	ptmx, err := pty.Start(command)
	if err != nil {
		return fmt.Errorf("iniciar PTY: %w", err)
	}
	defer ptmx.Close()

	waitDone := make(chan error, 1)
	go func() { waitDone <- command.Wait() }()
	transferDone := make(chan error, 2)
	go func() { transferDone <- copyPTYToWebSocket(ptmx, ws) }()
	go func() { transferDone <- copyWebSocketToPTY(ws, ptmx) }()

	select {
	case waitErr := <-waitDone:
		_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "shell exited"), time.Now().Add(time.Second))
		return normalizeExitError(waitErr)
	case transferErr := <-transferDone:
		_ = ptmx.Close()
		if command.Process != nil {
			_ = command.Process.Kill()
		}
		waitErr := <-waitDone
		if transferErr != nil && !isExpectedClose(transferErr) {
			return transferErr
		}
		return normalizeExitError(waitErr)
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

func setAuthorization(header http.Header, token string) {
	if token != "" {
		header.Set("Authorization", "Bearer "+token)
	}
}
