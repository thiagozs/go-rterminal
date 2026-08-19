package relay

import (
	"crypto/subtle"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/thiagozs/go-rterminal/internal/protocol"
)

const (
	registrationTimeout = 10 * time.Second
	maxMessageSize      = 1 << 20
)

type Config struct {
	AuthToken      string
	SessionTimeout time.Duration
	Logger         *log.Logger
}

type Server struct {
	authToken      string
	sessionTimeout time.Duration
	logger         *log.Logger
	upgrader       websocket.Upgrader

	mu       sync.Mutex
	agents   map[string]*agentConn
	sessions map[string]*session
}

type agentConn struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
}

func (a *agentConn) writeJSON(value any) error {
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	return a.conn.WriteJSON(value)
}

type session struct {
	id    string
	conns []*websocket.Conn
	timer *time.Timer
}

func New(config Config) *Server {
	logger := config.Logger
	if logger == nil {
		logger = log.Default()
	}
	return &Server{
		authToken:      config.AuthToken,
		sessionTimeout: config.SessionTimeout,
		logger:         logger,
		agents:         make(map[string]*agentConn),
		sessions:       make(map[string]*session),
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/agent", s.agentHandler)
	mux.HandleFunc("/client", s.clientHandler)
	mux.HandleFunc("/session", s.sessionHandler)
	mux.HandleFunc("/healthz", s.healthHandler)
	return mux
}

func (s *Server) authorized(r *http.Request) bool {
	if s.authToken == "" {
		return true
	}
	const prefix = "Bearer "
	header := r.Header.Get("Authorization")
	if len(header) <= len(prefix) || header[:len(prefix)] != prefix {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(header[len(prefix):]), []byte(s.authToken)) == 1
}

func (s *Server) ensureAuth(w http.ResponseWriter, r *http.Request) bool {
	if s.authorized(r) {
		return true
	}
	http.Error(w, "unauthorized", http.StatusUnauthorized)
	return false
}

func (s *Server) healthHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) agentHandler(w http.ResponseWriter, r *http.Request) {
	if !s.ensureAuth(w, r) {
		return
	}
	ws, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()

	ws.SetReadLimit(maxMessageSize)
	_ = ws.SetReadDeadline(time.Now().Add(registrationTimeout))
	var msg protocol.Message
	if err := ws.ReadJSON(&msg); err != nil {
		closeWebSocket(ws, websocket.ClosePolicyViolation, "invalid agent registration")
		return
	}
	// SessionID was used as the agent ID by versions prior to the shared protocol.
	if msg.AgentID == "" && msg.Type == "" {
		msg.AgentID = msg.SessionID
	}
	if msg.AgentID == "" || (msg.Type != "" && msg.Type != protocol.TypeRegisterAgent) {
		closeWebSocket(ws, websocket.ClosePolicyViolation, "invalid agent registration")
		return
	}
	_ = ws.SetReadDeadline(time.Time{})

	agent := &agentConn{conn: ws}
	if previous := s.setAgent(msg.AgentID, agent); previous != nil {
		closeWebSocket(previous.conn, websocket.CloseNormalClosure, "agent replaced")
		s.logger.Printf("[server] conexão anterior substituída | agent=%s", msg.AgentID)
	}
	defer s.removeAgent(msg.AgentID, agent)

	s.logger.Printf("[server] agente conectado | agent=%s", msg.AgentID)
	for {
		if _, _, err := ws.ReadMessage(); err != nil {
			s.logger.Printf("[server] agente desconectado | agent=%s", msg.AgentID)
			return
		}
	}
}

func (s *Server) setAgent(id string, agent *agentConn) *agentConn {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := s.agents[id]
	s.agents[id] = agent
	return previous
}

func (s *Server) removeAgent(id string, expected *agentConn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.agents[id] == expected {
		delete(s.agents, id)
	}
}

func (s *Server) clientHandler(w http.ResponseWriter, r *http.Request) {
	if !s.ensureAuth(w, r) {
		return
	}
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	agentID := r.URL.Query().Get("agent")
	if agentID == "" {
		http.Error(w, "agent is required", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	agent := s.agents[agentID]
	s.mu.Unlock()
	if agent == nil {
		http.Error(w, "agent offline", http.StatusNotFound)
		return
	}

	state := s.createSession()
	if err := agent.writeJSON(protocol.Message{Type: protocol.TypeStartSession, SessionID: state.id}); err != nil {
		s.removeAgent(agentID, agent)
		s.deleteSession(state.id, state)
		http.Error(w, "agent offline", http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(state.id))
}

func (s *Server) createSession() *session {
	state := &session{id: uuid.NewString()}
	s.mu.Lock()
	s.sessions[state.id] = state
	if s.sessionTimeout > 0 {
		state.timer = time.AfterFunc(s.sessionTimeout, func() { s.expireSession(state) })
	}
	s.mu.Unlock()
	return state
}

func (s *Server) deleteSession(id string, expected *session) {
	s.mu.Lock()
	if s.sessions[id] == expected {
		delete(s.sessions, id)
		if expected.timer != nil {
			expected.timer.Stop()
		}
	}
	s.mu.Unlock()
}

func (s *Server) expireSession(state *session) {
	s.mu.Lock()
	if s.sessions[state.id] != state {
		s.mu.Unlock()
		return
	}
	delete(s.sessions, state.id)
	connections := append([]*websocket.Conn(nil), state.conns...)
	s.mu.Unlock()

	s.logger.Printf("[server] sessão expirada | session=%s", state.id)
	for _, conn := range connections {
		closeWebSocket(conn, websocket.CloseNormalClosure, "pairing timeout")
	}
}

func (s *Server) sessionHandler(w http.ResponseWriter, r *http.Request) {
	if !s.ensureAuth(w, r) {
		return
	}
	id := r.URL.Query().Get("id")

	s.mu.Lock()
	state := s.sessions[id]
	full := state != nil && len(state.conns) >= 2
	s.mu.Unlock()
	if state == nil {
		http.Error(w, "invalid session", http.StatusNotFound)
		return
	}
	if full {
		http.Error(w, "session already paired", http.StatusConflict)
		return
	}

	ws, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	ws.SetReadLimit(maxMessageSize)

	var pair [2]*websocket.Conn
	s.mu.Lock()
	if s.sessions[id] != state || len(state.conns) >= 2 {
		s.mu.Unlock()
		closeWebSocket(ws, websocket.ClosePolicyViolation, "session unavailable")
		return
	}
	state.conns = append(state.conns, ws)
	if len(state.conns) == 2 {
		pair[0], pair[1] = state.conns[0], state.conns[1]
		delete(s.sessions, id)
		if state.timer != nil {
			state.timer.Stop()
		}
	}
	s.mu.Unlock()

	if pair[0] != nil {
		s.logger.Printf("[server] sessão pareada | session=%s", id)
		go s.bridgePair(id, pair[0], pair[1])
	}
}

func (s *Server) bridgePair(id string, a, b *websocket.Conn) {
	var once sync.Once
	closePair := func() {
		once.Do(func() {
			closeWebSocket(a, websocket.CloseNormalClosure, "session closed")
			closeWebSocket(b, websocket.CloseNormalClosure, "session closed")
			s.logger.Printf("[server] sessão encerrada | session=%s", id)
		})
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); defer closePair(); relayMessages(a, b) }()
	go func() { defer wg.Done(); defer closePair(); relayMessages(b, a) }()
	wg.Wait()
}

func relayMessages(src, dst *websocket.Conn) {
	for {
		messageType, data, err := src.ReadMessage()
		if err != nil {
			return
		}
		if err := dst.WriteMessage(messageType, data); err != nil {
			return
		}
	}
}

func closeWebSocket(conn *websocket.Conn, code int, reason string) {
	if conn == nil {
		return
	}
	deadline := time.Now().Add(time.Second)
	_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), deadline)
	_ = conn.Close()
}
