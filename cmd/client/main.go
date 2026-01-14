package main

import (
	"io"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/gorilla/websocket"
	"golang.org/x/term"
)

func main() {
	log.Println("[client] solicitando nova sessão")

	// 1) solicita nova sessão ao server
	resp, err := http.Get("http://127.0.0.1:8080/client?agent=agent-01")
	if err != nil {
		log.Fatal("[client] erro ao criar sessão:", err)
	}
	defer resp.Body.Close()

	sessionIDBytes, _ := io.ReadAll(resp.Body)
	sessionID := strings.TrimSpace(string(sessionIDBytes))

	log.Println("[client] sessão criada:", sessionID)

	// 2) conecta no websocket da sessão
	ws, _, err := websocket.DefaultDialer.Dial(
		"ws://127.0.0.1:8080/session?id="+sessionID,
		nil,
	)
	if err != nil {
		log.Fatal("[client] erro ao conectar sessão:", err)
	}
	defer ws.Close()

	log.Println("[client] conectado à sessão")

	// 3) coloca terminal em raw mode
	fd := int(os.Stdin.Fd())
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		log.Fatal("[client] erro ao ativar raw mode:", err)
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
}
