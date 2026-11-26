package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/spf13/cobra"
	"github.com/thiagozs/go-rterminal/internal/auth"
	"github.com/thiagozs/go-rterminal/internal/endpoint"
	"github.com/thiagozs/go-rterminal/internal/protocol"
	"github.com/thiagozs/go-rterminal/internal/tlsutil"
	"golang.org/x/term"
)

func main() {
	rootCmd := &cobra.Command{
		Use:           "client",
		Short:         "Conecta um cliente a uma sessao remota",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          runClient,
	}
	rootCmd.Flags().String("server-url", "http://127.0.0.1:8080", "URL HTTP do server")
	rootCmd.Flags().String("agent-id", "agent-01", "ID do agent alvo")
	rootCmd.Flags().String("auth-token", "", "Token de autenticacao (ou RTERMINAL_TOKEN)")
	rootCmd.Flags().String("tls-ca", "", "Caminho do CA para TLS (opcional)")
	rootCmd.Flags().Bool("tls-insecure", false, "Ignora verificacao TLS (nao recomendado)")
	cobra.CheckErr(rootCmd.Execute())
}

func runClient(cmd *cobra.Command, _ []string) error {
	serverURL, _ := cmd.Flags().GetString("server-url")
	agentID, _ := cmd.Flags().GetString("agent-id")
	flagToken, _ := cmd.Flags().GetString("auth-token")
	tlsCA, _ := cmd.Flags().GetString("tls-ca")
	tlsInsecure, _ := cmd.Flags().GetBool("tls-insecure")
	if agentID == "" {
		return fmt.Errorf("agent-id não pode ser vazio")
	}
	authToken, err := auth.ResolveToken(flagToken)
	if err != nil {
		return err
	}
	// Checked before asking the server for a session, so a non-interactive
	// run does not leave a shell open on the agent.
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return fmt.Errorf("stdin precisa ser um terminal")
	}

	tlsConfig, err := tlsutil.ClientConfig(tlsCA, tlsInsecure)
	if err != nil {
		return err
	}
	httpClient := &http.Client{
		Transport: &http.Transport{TLSClientConfig: tlsConfig},
		Timeout:   15 * time.Second,
	}

	clientURL, err := endpoint.URL(serverURL, "/client", url.Values{"agent": {agentID}}, false)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, clientURL, nil)
	if err != nil {
		return fmt.Errorf("criar requisição de sessão: %w", err)
	}
	auth.SetAuthorization(req.Header, authToken)

	log.Printf("[client] solicitando sessão | agent=%s", agentID)
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("criar sessão: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return fmt.Errorf("ler resposta da sessão: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("criar sessão: %s", strings.TrimSpace(string(body)))
	}
	sessionID := strings.TrimSpace(string(body))
	if sessionID == "" {
		return fmt.Errorf("servidor retornou uma sessão vazia")
	}

	sessionURL, err := endpoint.URL(serverURL, "/session", url.Values{"id": {sessionID}}, true)
	if err != nil {
		return err
	}
	headers := http.Header{}
	auth.SetAuthorization(headers, authToken)
	ws, _, err := (&websocket.Dialer{TLSClientConfig: tlsConfig}).Dial(sessionURL, headers)
	if err != nil {
		return fmt.Errorf("conectar à sessão: %w", err)
	}
	defer ws.Close()
	ws.SetReadLimit(1 << 20)

	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return fmt.Errorf("ativar raw mode: %w", err)
	}
	defer func() { _ = term.Restore(fd, oldState) }()

	writer := &wsWriter{conn: ws}
	sendResize := func() {
		if cols, rows, sizeErr := term.GetSize(fd); sizeErr == nil {
			_ = writer.writeJSON(protocol.Message{Type: protocol.TypeResize, Rows: uint16(rows), Cols: uint16(cols)})
		}
	}
	sendResize()
	stopResize := watchResize(sendResize)
	defer stopResize()

	inputDone := make(chan error, 1)
	go copyInput(writer, inputDone)

	log.Printf("[client] conectado | session=%s", sessionID)
	err = copyOutput(ws)
	select {
	case inputErr := <-inputDone:
		if err == nil {
			err = inputErr
		}
	default:
	}
	_, _ = os.Stdout.WriteString("\r\n[client] sessão encerrada.\r\n")
	return err
}

// wsWriter serializes writes, since input and resize events come from
// different goroutines and gorilla/websocket allows one concurrent writer.
type wsWriter struct {
	mu   sync.Mutex
	conn *websocket.Conn
}

func (w *wsWriter) writeJSON(v any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.conn.WriteJSON(v)
}

func (w *wsWriter) writeBinary(data []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.conn.WriteMessage(websocket.BinaryMessage, data)
}

func copyInput(ws *wsWriter, done chan<- error) {
	buffer := make([]byte, 4096)
	for {
		n, err := os.Stdin.Read(buffer)
		if err != nil {
			done <- err
			_ = ws.conn.Close()
			return
		}
		if err := ws.writeBinary(buffer[:n]); err != nil {
			done <- err
			return
		}
	}
}

func copyOutput(ws *websocket.Conn) error {
	for {
		messageType, data, err := ws.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				return nil
			}
			return fmt.Errorf("ler sessão: %w", err)
		}
		if messageType == websocket.BinaryMessage {
			if _, err := os.Stdout.Write(data); err != nil {
				return fmt.Errorf("escrever saída: %w", err)
			}
		}
	}
}
