package main

import (
	"crypto/subtle"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/spf13/cobra"
)

type Message struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id,omitempty"`
}

type Session struct {
	ID   string
	Once sync.Once
}

type agentConn struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

type sessionState struct {
	ch    chan *websocket.Conn
	timer *time.Timer
	mu    sync.Mutex
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

var (
	agents         sync.Map // agentID -> *agentConn
	sessions       sync.Map // sessionID -> *sessionState (pairing)
	authToken      string
	sessionTimeout time.Duration
)

func authorize(r *http.Request) bool {
	if authToken == "" {
		return true
	}
	header := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(header) <= len(prefix) || header[:len(prefix)] != prefix {
		return false
	}
	token := header[len(prefix):]
	return subtle.ConstantTimeCompare([]byte(token), []byte(authToken)) == 1
}

func ensureAuth(w http.ResponseWriter, r *http.Request) bool {
	if authorize(r) {
		return true
	}
	w.WriteHeader(http.StatusUnauthorized)
	w.Write([]byte("unauthorized"))
	return false
}

func agentHandler(w http.ResponseWriter, r *http.Request) {
	if !ensureAuth(w, r) {
		return
	}
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	var msg Message
	if err := ws.ReadJSON(&msg); err != nil {
		ws.Close()
		return
	}

	if prev, ok := agents.Load(msg.SessionID); ok {
		prevConn := prev.(*agentConn)
		prevConn.mu.Lock()
		err := prevConn.conn.WriteControl(
			websocket.PingMessage,
			[]byte("probe"),
			time.Now().Add(2*time.Second),
		)
		prevConn.mu.Unlock()
		if err == nil {
			ws.WriteMessage(
				websocket.CloseMessage,
				websocket.FormatCloseMessage(
					websocket.ClosePolicyViolation,
					"agent already connected",
				),
			)
			ws.Close()
			log.Println("Agent já conectado, rejeitando duplicado:", msg.SessionID)
			return
		}
	}
	agents.Store(msg.SessionID, &agentConn{conn: ws})
	log.Println("Agent conectado:", msg.SessionID)

	for {
		if _, _, err := ws.ReadMessage(); err != nil {
			agents.Delete(msg.SessionID)
			ws.Close()
			log.Println("Agent desconectado:", msg.SessionID)
			return
		}
	}
}

func clientHandler(w http.ResponseWriter, r *http.Request) {
	if !ensureAuth(w, r) {
		return
	}
	agentID := r.URL.Query().Get("agent")
	val, ok := agents.Load(agentID)
	if !ok {
		http.Error(w, "agent offline", 404)
		return
	}

	sessionID := uuid.NewString()
	state := &sessionState{
		ch: make(chan *websocket.Conn, 2),
	}
	sessions.Store(sessionID, state)

	// avisa o agent
	conn := val.(*agentConn)
	conn.mu.Lock()
	err := conn.conn.WriteJSON(Message{
		Type:      "start-session",
		SessionID: sessionID,
	})
	conn.mu.Unlock()
	if err != nil {
		agents.Delete(agentID)
		http.Error(w, "agent offline", 503)
		return
	}

	w.Write([]byte(sessionID))
}

func sessionHandler(w http.ResponseWriter, r *http.Request) {
	if !ensureAuth(w, r) {
		return
	}
	sessionID := r.URL.Query().Get("id")
	val, ok := sessions.Load(sessionID)
	if !ok {
		http.Error(w, "invalid session", 404)
		return
	}

	ws, _ := upgrader.Upgrade(w, r, nil)
	state := val.(*sessionState)
	state.ch <- ws

	state.mu.Lock()
	if state.timer == nil && sessionTimeout > 0 {
		state.timer = time.AfterFunc(sessionTimeout, func() {
			if len(state.ch) < 2 {
				sessions.Delete(sessionID)
				log.Printf("[server] sessão expirada | session=%s", sessionID)
				for len(state.ch) > 0 {
					conn := <-state.ch
					conn.WriteMessage(
						websocket.CloseMessage,
						websocket.FormatCloseMessage(
							websocket.CloseNormalClosure,
							"pairing timeout",
						),
					)
					conn.Close()
				}
			}
		})
	}
	state.mu.Unlock()

	if len(state.ch) == 2 {
		state.mu.Lock()
		if state.timer != nil {
			state.timer.Stop()
			state.timer = nil
		}
		state.mu.Unlock()

		a := <-state.ch
		b := <-state.ch
		sessions.Delete(sessionID)

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
	rootCmd := &cobra.Command{
		Use:   "server",
		Short: "Inicia o servidor do ShellHub",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runServer(cmd)
		},
	}

	rootCmd.Flags().String("addr", ":8080", "Endereco de escuta HTTP")
	rootCmd.Flags().String("auth-token", "change-me", "Token de autenticacao para agentes e clientes")
	rootCmd.Flags().String("tls-cert", "", "Caminho do certificado TLS (opcional)")
	rootCmd.Flags().String("tls-key", "", "Caminho da chave TLS (opcional)")
	rootCmd.Flags().Duration("session-timeout", 15*time.Second, "Timeout para pareamento de sessao")

	cobra.CheckErr(rootCmd.Execute())
}

func runServer(cmd *cobra.Command) error {
	addr, _ := cmd.Flags().GetString("addr")
	authToken, _ = cmd.Flags().GetString("auth-token")
	tlsCert, _ := cmd.Flags().GetString("tls-cert")
	tlsKey, _ := cmd.Flags().GetString("tls-key")
	sessionTimeout, _ = cmd.Flags().GetDuration("session-timeout")

	http.HandleFunc("/agent", agentHandler)
	http.HandleFunc("/client", clientHandler)
	http.HandleFunc("/session", sessionHandler)
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	log.Println("Server em", addr)
	if tlsCert != "" || tlsKey != "" {
		if tlsCert == "" || tlsKey == "" {
			return fmt.Errorf("tls-cert e tls-key devem ser informados juntos")
		}
		if err := http.ListenAndServeTLS(addr, tlsCert, tlsKey, nil); err != nil {
			return fmt.Errorf("erro ao iniciar server TLS: %w", err)
		}
		return nil
	}
	if err := http.ListenAndServe(addr, nil); err != nil {
		return fmt.Errorf("erro ao iniciar server: %w", err)
	}
	return nil
}
