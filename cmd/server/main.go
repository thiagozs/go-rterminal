package main

import (
	"log"
	"net/http"
	"sync"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

type Message struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id,omitempty"`
}

type Session struct {
	ID   string
	Once sync.Once
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

var (
	agents   sync.Map // agentID -> *websocket.Conn
	sessions sync.Map // sessionID -> chan *websocket.Conn (pairing)
)

func agentHandler(w http.ResponseWriter, r *http.Request) {
	ws, _ := upgrader.Upgrade(w, r, nil)

	var msg Message
	ws.ReadJSON(&msg)

	agents.Store(msg.SessionID, ws)
	log.Println("Agent conectado:", msg.SessionID)

	select {} // mantém vivo
}

func clientHandler(w http.ResponseWriter, r *http.Request) {
	agentID := r.URL.Query().Get("agent")
	val, ok := agents.Load(agentID)
	if !ok {
		http.Error(w, "agent offline", 404)
		return
	}

	sessionID := uuid.NewString()
	ch := make(chan *websocket.Conn, 2)
	sessions.Store(sessionID, ch)

	// avisa o agent
	val.(*websocket.Conn).WriteJSON(Message{
		Type:      "start-session",
		SessionID: sessionID,
	})

	w.Write([]byte(sessionID))
}

func sessionHandler(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("id")
	val, ok := sessions.Load(sessionID)
	if !ok {
		http.Error(w, "invalid session", 404)
		return
	}

	ws, _ := upgrader.Upgrade(w, r, nil)
	ch := val.(chan *websocket.Conn)
	ch <- ws

	if len(ch) == 2 {
		a := <-ch
		b := <-ch

		s := &Session{ID: sessionID}

		go bridge(a, b, s)
		go bridge(b, a, s)
	}
}

func bridge(src, dst *websocket.Conn, s *Session) {
	defer func() {
		s.Once.Do(func() {
			log.Printf("[server] sessão encerrada | session=%s", s.ID)
		})

		dst.WriteMessage(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(
				websocket.CloseNormalClosure,
				"session closed",
			),
		)

		dst.Close()
		src.Close()
	}()

	for {
		mt, data, err := src.ReadMessage()
		if err != nil {
			return
		}
		dst.WriteMessage(mt, data)
	}
}

func main() {
	http.HandleFunc("/agent", agentHandler)
	http.HandleFunc("/client", clientHandler)
	http.HandleFunc("/session", sessionHandler)

	log.Println("Server em :8080")
	http.ListenAndServe(":8080", nil)
}
