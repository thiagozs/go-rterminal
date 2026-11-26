package relay

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/thiagozs/go-rterminal/internal/protocol"
)

func TestAuthentication(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(New(Config{AuthToken: "secret"}).Handler())
	defer server.Close()

	response, err := http.Get(server.URL + "/client?agent=missing")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusUnauthorized)
	}
}

func TestSessionPairingRelaysMessages(t *testing.T) {
	t.Parallel()
	server := newTestServer(t, time.Second)
	agent := dialWebSocket(t, server.URL, "/agent", "secret")
	defer agent.Close()
	if err := agent.WriteJSON(protocol.Message{Type: protocol.TypeRegisterAgent, AgentID: "agent/a"}); err != nil {
		t.Fatal(err)
	}

	requestURL := server.URL + "/client?agent=" + url.QueryEscape("agent/a")
	request, err := http.NewRequest(http.MethodPost, requestURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer secret")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("create session status = %d, body = %s", response.StatusCode, body)
	}
	sessionID := strings.TrimSpace(string(body))

	var start protocol.Message
	if err := agent.ReadJSON(&start); err != nil {
		t.Fatal(err)
	}
	if start.Type != protocol.TypeStartSession || start.SessionID != sessionID {
		t.Fatalf("start message = %+v", start)
	}

	first := dialWebSocket(t, server.URL, "/session?id="+url.QueryEscape(sessionID), "secret")
	defer first.Close()
	second := dialWebSocket(t, server.URL, "/session?id="+url.QueryEscape(sessionID), "secret")
	defer second.Close()
	for _, conn := range []*websocket.Conn{first, second} { // pairing notice
		var notice protocol.Message
		if err := conn.ReadJSON(&notice); err != nil || notice.Type != protocol.TypePaired {
			t.Fatalf("pairing notice = %+v, err = %v", notice, err)
		}
	}
	payload := []byte("echo ok\n")
	if err := first.WriteMessage(websocket.BinaryMessage, payload); err != nil {
		t.Fatal(err)
	}
	messageType, got, err := second.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if messageType != websocket.BinaryMessage || string(got) != string(payload) {
		t.Fatalf("relayed message type=%d body=%q", messageType, got)
	}
}

func TestSessionExpiresWithoutPair(t *testing.T) {
	t.Parallel()
	relayServer := New(Config{
		AuthToken:      "secret",
		SessionTimeout: 20 * time.Millisecond,
		Logger:         log.New(io.Discard, "", 0),
	})
	state := relayServer.createSession()
	time.Sleep(100 * time.Millisecond)

	relayServer.mu.Lock()
	_, exists := relayServer.sessions[state.id]
	relayServer.mu.Unlock()
	if exists {
		t.Fatal("expired session remains registered")
	}
}

func TestOldConnectionCannotRemoveReplacement(t *testing.T) {
	t.Parallel()
	server := New(Config{})
	oldConnection := &agentConn{}
	newConnection := &agentConn{}
	replaced, ok := server.registerAgent("agent-01", oldConnection)
	if !ok || replaced != nil {
		t.Fatal("first registration should succeed without replacing")
	}
	// A connection that cannot be pinged is considered dead and replaced.
	replaced, ok = server.registerAgent("agent-01", newConnection)
	if !ok || replaced != oldConnection {
		t.Fatal("dead connection was not replaced")
	}
	server.removeAgent("agent-01", oldConnection)

	server.mu.Lock()
	got := server.agents["agent-01"]
	server.mu.Unlock()
	if got != newConnection {
		t.Fatal("cleanup from old connection removed its replacement")
	}
}

func TestLiveAgentCannotBeHijacked(t *testing.T) {
	t.Parallel()
	server := newTestServer(t, time.Second)
	legit := dialWebSocket(t, server.URL, "/agent", "secret")
	defer legit.Close()
	if err := legit.WriteJSON(protocol.Message{Type: protocol.TypeRegisterAgent, AgentID: "agent-01"}); err != nil {
		t.Fatal(err)
	}
	waitForAgent(t, server.URL, "agent-01")

	intruder := dialWebSocket(t, server.URL, "/agent", "secret")
	defer intruder.Close()
	if err := intruder.WriteJSON(protocol.Message{Type: protocol.TypeRegisterAgent, AgentID: "agent-01"}); err != nil {
		t.Fatal(err)
	}
	_ = intruder.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, err := intruder.ReadMessage()
	if !websocket.IsCloseError(err, websocket.ClosePolicyViolation) {
		t.Fatalf("intruder err = %v, want policy violation close", err)
	}

	request, _ := http.NewRequest(http.MethodPost, server.URL+"/client?agent=agent-01", nil)
	request.Header.Set("Authorization", "Bearer secret")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	var start protocol.Message
	_ = legit.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := legit.ReadJSON(&start); err != nil || start.Type != protocol.TypeStartSession {
		t.Fatalf("legitimate agent did not receive session: %+v, %v", start, err)
	}
}

func TestClientRequiresPost(t *testing.T) {
	t.Parallel()
	server := newTestServer(t, time.Second)
	request, _ := http.NewRequest(http.MethodGet, server.URL+"/client?agent=x", nil)
	request.Header.Set("Authorization", "Bearer secret")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", response.StatusCode)
	}
}

func waitForAgent(t *testing.T, baseURL, id string) {
	t.Helper()
	for i := 0; i < 50; i++ {
		request, _ := http.NewRequest(http.MethodPost, baseURL+"/client?agent="+id, nil)
		request.Header.Set("Authorization", "Bearer secret")
		response, err := http.DefaultClient.Do(request)
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("agent never registered")
}

func newTestServer(t *testing.T, timeout time.Duration) *httptest.Server {
	t.Helper()
	return httptest.NewServer(New(Config{
		AuthToken:      "secret",
		SessionTimeout: timeout,
		Logger:         log.New(io.Discard, "", 0),
	}).Handler())
}

func dialWebSocket(t *testing.T, baseURL, path, token string) *websocket.Conn {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(baseURL, "http") + path
	headers := http.Header{"Authorization": {"Bearer " + token}}
	conn, response, err := websocket.DefaultDialer.Dial(wsURL, headers)
	if err != nil {
		if response != nil {
			t.Fatalf("dial websocket: %v (status %d)", err, response.StatusCode)
		}
		t.Fatal(err)
	}
	return conn
}

func TestSilentAgentIsDropped(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(New(Config{
		AuthToken:         "secret",
		KeepAliveInterval: 50 * time.Millisecond,
		Logger:            log.New(io.Discard, "", 0),
	}).Handler())
	defer server.Close()

	// This client never reads, so it never answers the server's pings.
	agent := dialWebSocket(t, server.URL, "/agent", "secret")
	defer agent.Close()
	if err := agent.WriteJSON(protocol.Message{Type: protocol.TypeRegisterAgent, AgentID: "ghost"}); err != nil {
		t.Fatal(err)
	}
	waitForAgentWith(t, server.URL, "ghost", http.StatusOK)
	for i := 0; i < 50; i++ {
		if statusOfClient(t, server.URL, "ghost") == http.StatusNotFound {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("silent agent was never dropped")
}

func TestRespondingAgentStaysConnected(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(New(Config{
		AuthToken:         "secret",
		KeepAliveInterval: 50 * time.Millisecond,
		Logger:            log.New(io.Discard, "", 0),
	}).Handler())
	defer server.Close()

	agent := dialWebSocket(t, server.URL, "/agent", "secret")
	defer agent.Close()
	if err := agent.WriteJSON(protocol.Message{Type: protocol.TypeRegisterAgent, AgentID: "live"}); err != nil {
		t.Fatal(err)
	}
	go func() { // reading lets gorilla answer pings automatically
		for {
			if _, _, err := agent.ReadMessage(); err != nil {
				return
			}
		}
	}()
	time.Sleep(400 * time.Millisecond) // several keepalive periods
	if got := statusOfClient(t, server.URL, "live"); got != http.StatusOK {
		t.Fatalf("live agent status = %d, want 200", got)
	}
}

func statusOfClient(t *testing.T, baseURL, agentID string) int {
	t.Helper()
	request, _ := http.NewRequest(http.MethodPost, baseURL+"/client?agent="+agentID, nil)
	request.Header.Set("Authorization", "Bearer secret")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	return response.StatusCode
}

func waitForAgentWith(t *testing.T, baseURL, id string, want int) {
	t.Helper()
	for i := 0; i < 50; i++ {
		if statusOfClient(t, baseURL, id) == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("agent %s never reached status %d", id, want)
}
