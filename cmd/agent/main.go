package main

import (
	"log"
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

	cobra.CheckErr(rootCmd.Execute())
}

func runAgent(cmd *cobra.Command) error {
	serverURL, _ := cmd.Flags().GetString("server-url")
	agentID, _ := cmd.Flags().GetString("agent-id")
	shellPath, _ := cmd.Flags().GetString("shell")

	for {
		log.Println("[agent] tentando conectar ao server...")
		if err := run(serverURL, agentID, shellPath); err != nil {
			log.Println("[agent] conexão perdida:", err)
		}

		log.Println("[agent] tentando reconectar em 5s...")
		time.Sleep(5 * time.Second)
	}
}

func run(serverURL, agentID, shellPath string) error {
	ctrl, _, err := websocket.DefaultDialer.Dial(serverURL+"/agent", nil)
	if err != nil {
		return err
	}
	defer ctrl.Close()

	// registra o agent
	if err := ctrl.WriteJSON(Message{
		SessionID: agentID,
	}); err != nil {
		return err
	}

	log.Println("[agent] conectado ao server")

	for {
		var msg Message
		if err := ctrl.ReadJSON(&msg); err != nil {
			return err // força reconexão
		}

		if msg.Type == "start-session" {
			log.Println("[agent] iniciando sessão:", msg.SessionID)
			go startShell(serverURL, shellPath, msg.SessionID)
		}
	}
}

func startShell(serverURL, shellPath, sessionID string) {
	log.Println("[agent] sessão criada:", sessionID)

	ws, _, err := websocket.DefaultDialer.Dial(
		serverURL+"/session?id="+sessionID,
		nil,
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
