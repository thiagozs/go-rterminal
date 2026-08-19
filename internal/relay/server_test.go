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
	if previous := server.setAgent("agent-01", oldConnection); previous != nil {
		t.Fatal("first connection unexpectedly replaced another connection")
	}
	if previous := server.setAgent("agent-01", newConnection); previous != oldConnection {
		t.Fatal("replacement did not return the old connection")
	}
	server.removeAgent("agent-01", oldConnection)

	server.mu.Lock()
	got := server.agents["agent-01"]
	server.mu.Unlock()
	if got != newConnection {
		t.Fatal("cleanup from old connection removed its replacement")
	}
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
