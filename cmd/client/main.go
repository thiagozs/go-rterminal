package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/gorilla/websocket"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "client",
		Short: "Conecta um cliente a uma sessao remota",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runClient(cmd)
		},
	}

	rootCmd.Flags().String("server-url", "http://127.0.0.1:8080", "URL HTTP do server")
	rootCmd.Flags().String("agent-id", "agent-01", "ID do agent alvo")

	cobra.CheckErr(rootCmd.Execute())
}

func runClient(cmd *cobra.Command) error {
	serverURL, _ := cmd.Flags().GetString("server-url")
	agentID, _ := cmd.Flags().GetString("agent-id")

	log.Println("[client] solicitando nova sessão")

	// 1) solicita nova sessão ao server
	resp, err := http.Get(serverURL + "/client?agent=" + agentID)
	if err != nil {
		return fmt.Errorf("[client] erro ao criar sessão: %w", err)
	}
	defer resp.Body.Close()

	sessionIDBytes, _ := io.ReadAll(resp.Body)
	sessionID := strings.TrimSpace(string(sessionIDBytes))

	log.Println("[client] sessão criada:", sessionID)

	// 2) conecta no websocket da sessão
	ws, _, err := websocket.DefaultDialer.Dial(
		wsBaseURL(serverURL)+"/session?id="+sessionID,
		nil,
	)
	if err != nil {
		return fmt.Errorf("[client] erro ao conectar sessão: %w", err)
	}
	defer ws.Close()

	log.Println("[client] conectado à sessão")

	// 3) coloca terminal em raw mode
	fd := int(os.Stdin.Fd())
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return fmt.Errorf("[client] erro ao ativar raw mode: %w", err)
	}
	defer term.Restore(fd, oldState)

	done := make(chan struct{})

	// 4) stdin --> websocket
	go func() {
		buf := make([]byte, 1024)
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil {
				log.Println("[client] stdin fechado")
				close(done)
				return
			}

			err = ws.WriteMessage(websocket.BinaryMessage, buf[:n])
			if err != nil {
				log.Println("[client] erro ao enviar stdin:", err)
				close(done)
				return
			}
		}
	}()

	// 5) websocket --> stdout
	go func() {
		for {
			mt, data, err := ws.ReadMessage()
			if err != nil {
				log.Println("[client] sessão encerrada pelo server")
				close(done)
				return
			}

			switch mt {

			case websocket.BinaryMessage:
				_, _ = os.Stdout.Write(data)

			case websocket.CloseMessage:
				log.Println("[client] shell finalizado")
				close(done)
				return
			}
		}
	}()

	// 6)aguarda encerramento
	<-done

	// 7) restaura terminal e imprime mensagem final
	term.Restore(fd, oldState)
	os.Stdout.WriteString("\r\n[client] sessão encerrada.\r\n")
	return nil
}

func wsBaseURL(httpBase string) string {
	if after, ok := strings.CutPrefix(httpBase, "https://"); ok {
		return "wss://" + after
	}
	return "ws://" + strings.TrimPrefix(httpBase, "http://")
}
