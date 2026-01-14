package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/gorilla/websocket"
	"golang.org/x/term"
)

type ControlMessage struct {
	Type string `json:"type"`
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

	// sinais do SO
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	// stdin → websocket
	go func() {
		buf := make([]byte, 1024)
		for {
			select {
			case <-done:
				return
			default:
				n, err := os.Stdin.Read(buf)
				if err != nil {
					close(done)
					return
				}
				ws.WriteMessage(websocket.BinaryMessage, buf[:n])
			}
		}
	}()

	// websocket → terminal
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
				var ctrl ControlMessage
				if err := json.Unmarshal(data, &ctrl); err != nil {
					continue
				}
				if ctrl.Type == "end-session" {
					close(done)
					return
				}
			}
		}
	}()

	// aguarda fim
	select {
	case <-done:
	case <-sig:
	}

	// fecha websocket corretamente
	_ = ws.WriteMessage(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(
			websocket.CloseNormalClosure,
			"client closed",
		),
	)

	fmt.Printf("\nSession closed.")
}
