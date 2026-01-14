package main

import (
	"encoding/json"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/gorilla/websocket"
	"github.com/thiagozs/go-shellhub/internal/protocol"
	"golang.org/x/term"
)

func sendResize(ws *websocket.Conn) {
	cols, rows, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		return
	}

	msg := protocol.Message{
		Type: "resize",
		Cols: uint16(cols),
		Rows: uint16(rows),
	}

	data, _ := json.Marshal(msg)
	_ = ws.WriteMessage(websocket.TextMessage, data)
}

func main() {
	ws, _, err := websocket.DefaultDialer.Dial(
		"ws://127.0.0.1:8080/client?agent=agent-01",
		nil,
	)
	if err != nil {
		log.Fatal(err)
	}
	defer ws.Close()

	fd := int(os.Stdin.Fd())
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		log.Fatal(err)
	}
	defer term.Restore(fd, oldState)

	done := make(chan struct{})

	// sinais
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGWINCH)

	// resize inicial
	sendResize(ws)

	// stdin → ws
	go func() {
		buf := make([]byte, 1024)
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil {
				close(done)
				return
			}
			_ = ws.WriteMessage(websocket.BinaryMessage, buf[:n])
		}
	}()

	// ws → terminal
	go func() {
		for {
			mt, data, err := ws.ReadMessage()
			if err != nil {
				close(done)
				return
			}

			switch mt {
			case websocket.BinaryMessage:
				os.Stdout.Write(data)

			case websocket.TextMessage:
				var msg protocol.Message
				if err := json.Unmarshal(data, &msg); err != nil {
					continue
				}

				if msg.Type == "end-session" {
					close(done)
					return
				}
			}
		}
	}()

	for {
		select {
		case s := <-sig:
			if s == syscall.SIGWINCH {
				sendResize(ws)
			} else {
				close(done)
				goto exit
			}

		case <-done:
			goto exit
		}
	}

exit:
	_ = ws.WriteMessage(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(
			websocket.CloseNormalClosure,
			"client closed",
		),
	)

	// 🔑 RESTAURA TERMINAL PRIMEIRO
	term.Restore(fd, oldState)

	// 🔑 PRINT LIMPO
	os.Stdout.WriteString("\r\nSession closed.\r\n")
}
