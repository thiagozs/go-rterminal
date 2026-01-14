package main

import (
	"log"
	"os/exec"
	"time"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
)

type Message struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id,omitempty"`
}

const (
	serverURL = "ws://127.0.0.1:8080"
	agentID   = "agent-01"
)

func main() {
	for {
		log.Println("[agent] tentando conectar ao server...")
		if err := run(); err != nil {
			log.Println("[agent] conexão perdida:", err)
		}

		log.Println("[agent] tentando reconectar em 5s...")
		time.Sleep(5 * time.Second)
	}
}

func run() error {
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
			go startShell(msg.SessionID)
		}
	}
}

func startShell(sessionID string) {
	log.Println("[agent] sessão criada:", sessionID)

	ws, _, err := websocket.DefaultDialer.Dial(
		serverURL+"/session?id="+sessionID,
		nil,
	)
	if err != nil {
		log.Println("[agent] erro ao conectar sessão:", err)
		return
	}

	cmd := exec.Command("/bin/bash")
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
