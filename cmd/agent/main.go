package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"time"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
	"github.com/spf13/cobra"
)

type Message struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id,omitempty"`
}

func main() {
	rootCmd := &cobra.Command{
		Use:   "agent",
		Short: "Conecta um agente ao servidor do ShellHub",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAgent(cmd)
		},
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

func runAgent(cmd *cobra.Command) error {
	serverURL, _ := cmd.Flags().GetString("server-url")
	agentID, _ := cmd.Flags().GetString("agent-id")
	shellPath, _ := cmd.Flags().GetString("shell")
	authToken, _ := cmd.Flags().GetString("auth-token")
	tlsCA, _ := cmd.Flags().GetString("tls-ca")
	tlsInsecure, _ := cmd.Flags().GetBool("tls-insecure")
	backoffMin, _ := cmd.Flags().GetDuration("backoff-min")
	backoffMax, _ := cmd.Flags().GetDuration("backoff-max")
	keepaliveInterval, _ := cmd.Flags().GetDuration("keepalive-interval")

	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	backoff := newBackoff(backoffMin, backoffMax, rng)

	for {
		log.Println("[agent] tentando conectar ao server...")
		if err := run(serverURL, agentID, shellPath, authToken, tlsCA, tlsInsecure, keepaliveInterval); err != nil {
			log.Println("[agent] conexão perdida:", err)
			sleep := backoff.Next()
			log.Printf("[agent] tentando reconectar em %s...", sleep)
			time.Sleep(sleep)
			continue
		}
		backoff.Reset()
	}
}

func run(serverURL, agentID, shellPath, authToken, tlsCA string, tlsInsecure bool, keepaliveInterval time.Duration) error {
	tlsConfig, err := tlsConfigFromFlags(tlsCA, tlsInsecure)
	if err != nil {
		return err
	}
	headers := http.Header{}
	if authToken != "" {
		headers.Set("Authorization", "Bearer "+authToken)
	}

	dialer := websocket.Dialer{TLSClientConfig: tlsConfig}
	ctrl, _, err := dialer.Dial(serverURL+"/agent", headers)
	if err != nil {
		return err
	}
	defer ctrl.Close()
	defer ctrl.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "agent disconnect"))

	// registra o agent
	if err := ctrl.WriteJSON(Message{
		SessionID: agentID,
	}); err != nil {
		return err
	}

	log.Println("[agent] conectado ao server")

	if keepaliveInterval > 0 {
		// Keepalive do controle para evitar desconexoes silenciosas.
		ctrl.SetReadDeadline(time.Now().Add(keepaliveInterval * 2))
		ctrl.SetPongHandler(func(string) error {
			ctrl.SetReadDeadline(time.Now().Add(keepaliveInterval * 2))
			return nil
		})

		keepaliveDone := make(chan struct{})
		go func() {
			ticker := time.NewTicker(keepaliveInterval)
			defer ticker.Stop()
			for {
				select {
				case <-keepaliveDone:
					return
				case <-ticker.C:
					if err := ctrl.WriteControl(websocket.PingMessage, []byte("ping"), time.Now().Add(5*time.Second)); err != nil {
						ctrl.Close()
						return
					}
				}
			}
		}()
		defer close(keepaliveDone)
	}

	for {
		var msg Message
		if err := ctrl.ReadJSON(&msg); err != nil {
			return err // força reconexão
		}

		if msg.Type == "start-session" {
			log.Println("[agent] iniciando sessão:", msg.SessionID)
			go startShell(serverURL, shellPath, msg.SessionID, authToken, tlsConfig)
		}
	}
}

func startShell(serverURL, shellPath, sessionID, authToken string, tlsConfig *tls.Config) {
	log.Println("[agent] sessão criada:", sessionID)

	headers := http.Header{}
	if authToken != "" {
		headers.Set("Authorization", "Bearer "+authToken)
	}

	dialer := websocket.Dialer{TLSClientConfig: tlsConfig}
	ws, _, err := dialer.Dial(
		serverURL+"/session?id="+sessionID,
		headers,
	)
	if err != nil {
		log.Println("[agent] erro ao conectar sessão:", err)
		return
	}

	cmd := exec.Command(shellPath)
	ptmx, err := pty.Start(cmd)
	if err != nil {
		log.Println("[agent] erro ao iniciar PTY:", err)
		ws.Close()
		return
	}

	// PTY --> WS
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			if err != nil {
				return
			}
			ws.WriteMessage(websocket.BinaryMessage, buf[:n])
		}
	}()

	// WS --> PTY
	go func() {
		for {
			_, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			ptmx.Write(data)
		}
	}()

	// aguarda shell encerrar
	err = cmd.Wait()
	log.Printf("[agent] shell encerrado | session=%s | err=%v", sessionID, err)

	// fecha websocket da sessão (desbloqueia client)
	ws.WriteMessage(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(
			websocket.CloseNormalClosure,
			"shell exited",
		),
	)
	ws.Close()
}

func tlsConfigFromFlags(caPath string, insecure bool) (*tls.Config, error) {
	if caPath == "" {
		return &tls.Config{InsecureSkipVerify: insecure}, nil
	}

	caCert, err := os.ReadFile(caPath)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caCert) {
		return nil, fmt.Errorf("falha ao carregar CA")
	}

	return &tls.Config{
		InsecureSkipVerify: insecure,
		RootCAs:            pool,
	}, nil
}

type backoffState struct {
	min time.Duration
	max time.Duration
	cur time.Duration
	rng *rand.Rand
}

func newBackoff(min, max time.Duration, rng *rand.Rand) *backoffState {
	return &backoffState{
		min: min,
		max: max,
		cur: min,
		rng: rng,
	}
}

func (b *backoffState) Next() time.Duration {
	if b.cur <= 0 {
		b.cur = b.min
	}
	maxSleep := b.cur
	if maxSleep > b.max {
		maxSleep = b.max
	}
	jitter := time.Duration(b.rng.Int63n(int64(maxSleep) + 1))
	if b.cur < b.max {
		b.cur *= 2
	}
	if b.cur > b.max {
		b.cur = b.max
	}
	return jitter
}

func (b *backoffState) Reset() {
	b.cur = b.min
}
