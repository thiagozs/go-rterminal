package main

import (
	"log"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/thiagozs/go-shellhub/internal/protocol"
)

var (
	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	agents sync.Map // agentID -> *websocket.Conn
)

func agentHandler(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	var msg protocol.Message
	if err := conn.ReadJSON(&msg); err != nil || msg.Type != "register" {
		log.Println("Registro inválido de agent")
		conn.Close()
		return
	}

	log.Println("Agent conectado:", msg.AgentID)
	agents.Store(msg.AgentID, conn)

	// O relay será responsável por consumir esse socket
}

func clientHandler(w http.ResponseWriter, r *http.Request) {
	agentID := r.URL.Query().Get("agent")
	val, ok := agents.Load(agentID)
	if !ok {
		http.Error(w, "agent offline", http.StatusNotFound)
		return
	}

	clientConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	agentConn := val.(*websocket.Conn)

	log.Println("Cliente conectado ao agent:", agentID)

	// AVISA O AGENT PARA INICIAR UMA NOVA SESSÃO
	err = agentConn.WriteJSON(protocol.Message{
		Type: "start-session",
	})
	if err != nil {
		log.Println("Erro ao iniciar sessão:", err)
		clientConn.Close()
		return
	}

	// relay de dados
	go relay(clientConn, agentConn)
	go relay(agentConn, clientConn)
}

func relay(src, dst *websocket.Conn) {
	for {
		mt, data, err := src.ReadMessage()
		if err != nil {
			src.Close()
			dst.Close()
			return
		}
		if err := dst.WriteMessage(mt, data); err != nil {
			src.Close()
			dst.Close()
			return
		}
	}
}

func main() {
	http.HandleFunc("/agent", agentHandler)
	http.HandleFunc("/client", clientHandler)

	log.Println("Server em :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
