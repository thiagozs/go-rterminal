package main

import (
	"encoding/json"
	"errors"
	"log"
	"os/exec"
	"sync"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
	"github.com/thiagozs/go-shellhub/internal/protocol"
)

func main() {
	for {
		err := runAgent()
		if err != nil {
			// EOF / close normal → ignora
			if websocket.IsCloseError(err,
				websocket.CloseNormalClosure,
				websocket.CloseGoingAway,
				websocket.CloseAbnormalClosure,
			) || errors.Is(err, websocket.ErrCloseSent) {
				log.Println("Sessão encerrada, mantendo agent online")
				continue
			}

			log.Println("Agent error:", err)
		}

		log.Println("Reiniciando conexão do agent...")
	}

}

func runAgent() error {
	conn, _, err := websocket.DefaultDialer.Dial(
		"ws://127.0.0.1:8080/agent", nil,
	)
	if err != nil {
		return err
	}
	defer conn.Close()

	// Registro do agent
	if err := conn.WriteJSON(protocol.Message{
		Type:    "register",
		AgentID: "agent-01",
		Token:   "secret",
	}); err != nil {
		return err
	}

	log.Println("Agent online e aguardando sessão...")

	for {
		// Espera qualquer dado do server
		// iniciar sessão
		var msg protocol.Message
		if err := conn.ReadJSON(&msg); err != nil {
			return err
		}

		if msg.Type != "start-session" {
			continue
		}

		log.Println("Iniciando nova sessão shell")
		if err := runShellSession(conn); err != nil {
			log.Println("Sessão encerrada:", err)
		}
	}
}

func runShellSession(conn *websocket.Conn) error {
	cmd := exec.Command("/bin/bash")
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return err
	}
	defer ptmx.Close()

	done := make(chan struct{})
	var once sync.Once
	closeDone := func() { once.Do(func() { close(done) }) }

	// PTY -> WS
	go func() {
		buf := make([]byte, 1024)
		for {
			n, err := ptmx.Read(buf)
			if err != nil {
				closeDone()
				return
			}
			if err := conn.WriteMessage(websocket.BinaryMessage, buf[:n]); err != nil {
				closeDone()
				return
			}
		}
	}()

	// WS -> PTY (dados + controle)
	go func() {
		for {
			mt, data, err := conn.ReadMessage()
			if err != nil {
				closeDone()
				return
			}

			// controle
			if mt == websocket.TextMessage {
				var msg protocol.Message
				if err := json.Unmarshal(data, &msg); err != nil {
					continue
				}

				if msg.Type == "resize" {
					pty.Setsize(ptmx, &pty.Winsize{
						Cols: msg.Cols,
						Rows: msg.Rows,
					})
				}
				continue
			}

			// dados
			if _, err := ptmx.Write(data); err != nil {
				closeDone()
				return
			}
		}
	}()

	// Aguarda bash encerrar
	go func() {
		cmd.Wait()
		closeDone()
	}()

	<-done

	_ = conn.WriteJSON(protocol.Message{
		Type: "end-session",
	})

	// sessão terminou, mas agent continua
	log.Println("Shell encerrado")
	return nil
}
