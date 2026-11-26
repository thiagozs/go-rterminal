package relay

import (
	"crypto/subtle"
	"errors"
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
	writeTimeout        = 10 * time.Second

	// DefaultKeepAliveInterval is how often the server pings agents and
	// paired sessions; a peer that stays silent for twice this long is dropped.
	DefaultKeepAliveInterval = 20 * time.Second
)

type Config struct {
	// AuthToken is a shared secret. Without AgentAuth it authenticates every
	// endpoint (legacy mode); with AgentAuth it only authenticates CLI clients.
	AuthToken string
	// AgentAuth, when set, switches agents to per-machine credentials: it
	// resolves a bearer token to the machine id it belongs to.
	AgentAuth func(token string) (agentID string, ok bool)
	// OnAgentConnected / OnAgentDisconnected observe agent registrations.
	OnAgentConnected    func(id string, info protocol.Message)
	OnAgentDisconnected func(id string)
	SessionTimeout      time.Duration
	// KeepAliveInterval controls liveness pings; zero uses
	// DefaultKeepAliveInterval and a negative value disables them.
	KeepAliveInterval time.Duration
	Logger            *log.Logger
}

type Server struct {
	authToken      string
	agentAuth      func(token string) (string, bool)
	onConnected    func(id string, info protocol.Message)
	onDisconnected func(id string)
	sessionTimeout time.Duration
	keepAlive      time.Duration
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
	_ = a.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	return a.conn.WriteJSON(value)
}

// alive reports whether a ping can still be written to the connection.
func (a *agentConn) alive() bool {
	if a.conn == nil {
		return false
	}
	return a.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(time.Second)) == nil
}

type session struct {
	id      string
	agentID string // machine the session was opened for; "" = any (legacy)
	conns   []*websocket.Conn
	timer   *time.Timer
}

func New(config Config) *Server {
	logger := config.Logger
	if logger == nil {
		logger = log.Default()
	}
	keepAlive := config.KeepAliveInterval
	if keepAlive == 0 {
		keepAlive = DefaultKeepAliveInterval
	}
	return &Server{
		keepAlive:      keepAlive,
		authToken:      config.AuthToken,
		agentAuth:      config.AgentAuth,
		onConnected:    config.OnAgentConnected,
		onDisconnected: config.OnAgentDisconnected,
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

func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	header := r.Header.Get("Authorization")
	if len(header) <= len(prefix) || header[:len(prefix)] != prefix {
		return ""
	}
	return header[len(prefix):]
}

func (s *Server) sharedTokenMatches(token string) bool {
	return s.authToken != "" && token != "" &&
		subtle.ConstantTimeCompare([]byte(token), []byte(s.authToken)) == 1
}

// authorizedShared authenticates CLI clients (and, in legacy mode, agents)
// with the shared token. Legacy mode with no token configured is open.
func (s *Server) authorizedShared(r *http.Request) bool {
	if s.agentAuth == nil && s.authToken == "" {
		return true
	}
	return s.sharedTokenMatches(bearerToken(r))
}

func (s *Server) ensureAuth(w http.ResponseWriter, r *http.Request) bool {
	if s.authorizedShared(r) {
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
	// With per-machine credentials the token itself names the agent.
	machineID := ""
	if s.agentAuth != nil {
		id, ok := s.agentAuth(bearerToken(r))
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		machineID = id
	} else if !s.ensureAuth(w, r) {
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
	if machineID != "" {
		if msg.AgentID != "" && msg.AgentID != machineID {
			closeWebSocket(ws, websocket.ClosePolicyViolation, "agent id does not match credential")
			return
		}
		msg.AgentID = machineID
		msg.Type = protocol.TypeRegisterAgent
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
	replaced, ok := s.registerAgent(msg.AgentID, agent)
	if !ok {
		s.logger.Printf("[server] registro recusado, agente já conectado | agent=%s", msg.AgentID)
		closeWebSocket(ws, websocket.ClosePolicyViolation, "agent already connected")
		return
	}
	if replaced != nil {
		closeWebSocket(replaced.conn, websocket.CloseNormalClosure, "agent replaced")
		s.logger.Printf("[server] conexão inativa substituída | agent=%s", msg.AgentID)
	}
	defer func() {
		if s.removeAgent(msg.AgentID, agent) && s.onDisconnected != nil {
			s.onDisconnected(msg.AgentID)
		}
	}()

	s.logger.Printf("[server] agente conectado | agent=%s", msg.AgentID)
	if s.onConnected != nil {
		s.onConnected(msg.AgentID, msg)
	}
	defer s.startKeepAlive(ws)()
	for {
		if _, _, err := ws.ReadMessage(); err != nil {
			s.logger.Printf("[server] agente desconectado | agent=%s", msg.AgentID)
			return
		}
	}
}

// registerAgent claims id for agent. An existing registration is kept (and ok
// is false) while its connection still answers a ping, so a token holder
// cannot hijack a live agent; a dead connection is replaced and returned.
func (s *Server) registerAgent(id string, agent *agentConn) (replaced *agentConn, ok bool) {
	for {
		s.mu.Lock()
		previous := s.agents[id]
		if previous == nil {
			s.agents[id] = agent
			s.mu.Unlock()
			return nil, true
		}
		s.mu.Unlock()

		if previous.alive() {
			return nil, false
		}
		s.mu.Lock()
		if s.agents[id] == previous {
			s.agents[id] = agent
			s.mu.Unlock()
			return previous, true
		}
		s.mu.Unlock()
	}
}

// removeAgent unregisters expected and reports whether it was still current.
func (s *Server) removeAgent(id string, expected *agentConn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.agents[id] == expected {
		delete(s.agents, id)
		return true
	}
	return false
}

// IsOnline reports whether an agent is registered under id.
func (s *Server) IsOnline(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.agents[id] != nil
}

// Disconnect drops the agent registered under id, e.g. after its credential
// was revoked or rotated.
func (s *Server) Disconnect(id string, reason string) {
	s.mu.Lock()
	agent := s.agents[id]
	s.mu.Unlock()
	if agent != nil {
		closeWebSocket(agent.conn, websocket.ClosePolicyViolation, reason)
	}
}

// ErrAgentOffline is returned when no agent is registered for a machine.
var ErrAgentOffline = errors.New("agent offline")

// OpenTerminal asks the agent for machineID to start a session and attaches
// conn (an already upgraded WebSocket, e.g. a browser terminal) as the first
// end. The relay owns conn from here on; the agent joins as the second end.
func (s *Server) OpenTerminal(machineID string, conn *websocket.Conn) (string, error) {
	s.mu.Lock()
	agent := s.agents[machineID]
	s.mu.Unlock()
	if agent == nil {
		return "", ErrAgentOffline
	}
	conn.SetReadLimit(maxMessageSize)
	state := s.createSession(machineID)
	s.mu.Lock()
	state.conns = append(state.conns, conn)
	s.mu.Unlock()
	if err := agent.writeJSON(protocol.Message{Type: protocol.TypeStartSession, SessionID: state.id}); err != nil {
		s.removeAgent(machineID, agent)
		s.deleteSession(state.id, state)
		return "", ErrAgentOffline
	}
	return state.id, nil
}

func (s *Server) clientHandler(w http.ResponseWriter, r *http.Request) {
	if !s.ensureAuth(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
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

	state := s.createSession(agentID)
	if err := agent.writeJSON(protocol.Message{Type: protocol.TypeStartSession, SessionID: state.id}); err != nil {
		s.removeAgent(agentID, agent)
		s.deleteSession(state.id, state)
		http.Error(w, "agent offline", http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(state.id))
}

func (s *Server) createSession(agentID string) *session {
	state := &session{id: uuid.NewString(), agentID: agentID}
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
	// Sessions accept the shared token (CLI clients) or, in credential mode, the
	// credential of the machine the session was opened for.
	machineID := ""
	if !s.authorizedShared(r) {
		id, ok := "", false
		if s.agentAuth != nil {
			id, ok = s.agentAuth(bearerToken(r))
		}
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		machineID = id
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
	if machineID != "" && state.agentID != machineID {
		http.Error(w, "session belongs to another agent", http.StatusForbidden)
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
	defer s.startKeepAlive(a)()
	defer s.startKeepAlive(b)()
	// Tell both ends the peer is there; the agent only starts a shell now.
	paired := protocol.Message{Type: protocol.TypePaired, SessionID: id}
	for _, conn := range []*websocket.Conn{a, b} {
		_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
		if err := conn.WriteJSON(paired); err != nil {
			closePair()
			return
		}
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); defer closePair(); s.relayMessages(a, b) }()
	go func() { defer wg.Done(); defer closePair(); s.relayMessages(b, a) }()
	wg.Wait()
}

func (s *Server) relayMessages(src, dst *websocket.Conn) {
	for {
		messageType, data, err := src.ReadMessage()
		if err != nil {
			forwardClose(dst, err)
			return
		}
		s.extendReadDeadline(src)
		_ = dst.SetWriteDeadline(time.Now().Add(writeTimeout))
		if err := dst.WriteMessage(messageType, data); err != nil {
			return
		}
	}
}

// startKeepAlive pings conn periodically and arms a read deadline that every
// pong (or relayed message) pushes forward, so half-open peers are detected
// by the connection's read loop. It must be called before that loop starts
// reading and returns a function that stops the pinger.
func (s *Server) startKeepAlive(conn *websocket.Conn) (stop func()) {
	if s.keepAlive < 0 {
		return func() {}
	}
	s.extendReadDeadline(conn)
	conn.SetPongHandler(func(string) error {
		s.extendReadDeadline(conn)
		return nil
	})
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(s.keepAlive)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(time.Second)) != nil {
					_ = conn.Close()
					return
				}
			}
		}
	}()
	return func() { close(done) }
}

func (s *Server) extendReadDeadline(conn *websocket.Conn) {
	if s.keepAlive > 0 {
		_ = conn.SetReadDeadline(time.Now().Add(2 * s.keepAlive))
	}
}

// forwardClose passes a peer's close code and reason on to the other end, so
// reasons such as "agent busy" reach the client. Codes that must not appear on
// the wire (no status, abnormal closure, TLS failure) are not forwarded.
func forwardClose(dst *websocket.Conn, err error) {
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) {
		return
	}
	switch closeErr.Code {
	case websocket.CloseNoStatusReceived, websocket.CloseAbnormalClosure, websocket.CloseTLSHandshake:
		return
	}
	closeWebSocket(dst, closeErr.Code, closeErr.Text)
}

func closeWebSocket(conn *websocket.Conn, code int, reason string) {
	if conn == nil {
		return
	}
	deadline := time.Now().Add(time.Second)
	_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), deadline)
	_ = conn.Close()
}
