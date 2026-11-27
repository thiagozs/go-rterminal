package relay

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/thiagozs/go-rterminal/internal/protocol"
)

type hookLog struct {
	mu           sync.Mutex
	connected    []string
	disconnected []string
	info         protocol.Message
}

func (h *hookLog) snapshot() (c, d []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.connected...), append([]string(nil), h.disconnected...)
}

func newCredentialServer(t *testing.T, hooks *hookLog) (*Server, *httptest.Server) {
	t.Helper()
	tokens := map[string]string{"tok-a": "machine-a", "tok-b": "machine-b"}
	relay := New(Config{
		AuthToken: "shared",
		AgentAuth: func(token string) (string, bool) { id, ok := tokens[token]; return id, ok },
		OnAgentConnected: func(id string, info protocol.Message) {
			hooks.mu.Lock()
			hooks.connected = append(hooks.connected, id)
			hooks.info = info
			hooks.mu.Unlock()
		},
		OnAgentDisconnected: func(id, _ string) {
			hooks.mu.Lock()
			hooks.disconnected = append(hooks.disconnected, id)
			hooks.mu.Unlock()
		},
		SessionTimeout: 2 * time.Second,
		Logger:         log.New(io.Discard, "", 0),
	})
	server := httptest.NewServer(relay.Handler())
	t.Cleanup(server.Close)
	return relay, server
}

func registerAs(t *testing.T, baseURL, token string, msg protocol.Message) *websocket.Conn {
	t.Helper()
	conn := dialWebSocket(t, baseURL, "/agent", token)
	t.Cleanup(func() { conn.Close() })
	if err := conn.WriteJSON(msg); err != nil {
		t.Fatal(err)
	}
	return conn
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestCredentialModeRejectsUnknownAndSharedTokens(t *testing.T) {
	t.Parallel()
	_, server := newCredentialServer(t, &hookLog{})
	for _, token := range []string{"", "nope", "shared"} {
		wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/agent"
		headers := http.Header{}
		if token != "" {
			headers.Set("Authorization", "Bearer "+token)
		}
		_, response, err := websocket.DefaultDialer.Dial(wsURL, headers)
		if err == nil || response == nil || response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("token %q: err=%v response=%v, want 401", token, err, response)
		}
	}
}

func TestAgentIdentityComesFromCredential(t *testing.T) {
	t.Parallel()
	hooks := &hookLog{}
	relay, server := newCredentialServer(t, hooks)

	// The registration message cannot claim another machine's id.
	liar := registerAs(t, server.URL, "tok-a", protocol.Message{Type: protocol.TypeRegisterAgent, AgentID: "machine-b"})
	_ = liar.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := liar.ReadMessage(); !websocket.IsCloseError(err, websocket.ClosePolicyViolation) {
		t.Fatalf("mismatched id err = %v, want policy violation", err)
	}

	// Without an agent_id the credential decides, and facts reach the hook.
	registerAs(t, server.URL, "tok-a", protocol.Message{Type: protocol.TypeRegisterAgent, Hostname: "pi", OS: "linux", Arch: "arm64"})
	waitFor(t, "machine-a online", func() bool { return relay.IsOnline("machine-a") })
	connected, _ := hooks.snapshot()
	if len(connected) != 1 || connected[0] != "machine-a" || hooks.info.Hostname != "pi" || hooks.info.Arch != "arm64" {
		t.Fatalf("hook connected=%v info=%+v", connected, hooks.info)
	}
}

func TestDisconnectFiresHookOnlyForCurrentConnection(t *testing.T) {
	t.Parallel()
	hooks := &hookLog{}
	relay, server := newCredentialServer(t, hooks)
	conn := registerAs(t, server.URL, "tok-a", protocol.Message{Type: protocol.TypeRegisterAgent})
	waitFor(t, "online", func() bool { return relay.IsOnline("machine-a") })

	relay.Disconnect("machine-a", "credential revoked")
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := conn.ReadMessage(); !websocket.IsCloseError(err, websocket.ClosePolicyViolation) {
		t.Fatalf("kick err = %v", err)
	}
	waitFor(t, "disconnect hook", func() bool { _, d := hooks.snapshot(); return len(d) == 1 })
	if relay.IsOnline("machine-a") {
		t.Fatal("agent still online after Disconnect")
	}
}

func TestOpenTerminalPairsBrowserWithAgent(t *testing.T) {
	t.Parallel()
	hooks := &hookLog{}
	relay, server := newCredentialServer(t, hooks)
	agent := registerAs(t, server.URL, "tok-a", protocol.Message{Type: protocol.TypeRegisterAgent})
	waitFor(t, "online", func() bool { return relay.IsOnline("machine-a") })

	// A stand-in for the browser: a WebSocket the "admin" handler already upgraded.
	browserSide := make(chan *websocket.Conn, 1)
	adminSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		if _, err := relay.OpenTerminal("machine-a", conn, nil); err != nil {
			t.Errorf("OpenTerminal: %v", err)
		}
		browserSide <- conn
	}))
	defer adminSrv.Close()
	browser, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(adminSrv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	<-browserSide

	var start protocol.Message
	_ = agent.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := agent.ReadJSON(&start); err != nil || start.Type != protocol.TypeStartSession {
		t.Fatalf("start = %+v, %v", start, err)
	}

	// Another machine cannot take the session; the right one can.
	req, _ := http.NewRequest(http.MethodGet, server.URL+"/session?id="+url.QueryEscape(start.SessionID), nil)
	req.Header.Set("Authorization", "Bearer tok-b")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("other machine status = %d, want 403", resp.StatusCode)
	}
	session := dialWebSocket(t, server.URL, "/session?id="+url.QueryEscape(start.SessionID), "tok-a")
	defer session.Close()

	for _, conn := range []*websocket.Conn{browser, session} {
		var notice protocol.Message
		if err := conn.ReadJSON(&notice); err != nil || notice.Type != protocol.TypePaired {
			t.Fatalf("pairing notice = %+v, %v", notice, err)
		}
	}
	if err := browser.WriteMessage(websocket.BinaryMessage, []byte("ls\n")); err != nil {
		t.Fatal(err)
	}
	_, got, err := session.ReadMessage()
	if err != nil || string(got) != "ls\n" {
		t.Fatalf("relayed = %q, %v", got, err)
	}
}

func TestOpenTerminalOfflineAgent(t *testing.T) {
	t.Parallel()
	relay, _ := newCredentialServer(t, &hookLog{})
	if _, err := relay.OpenTerminal("machine-a", nil, nil); err != ErrAgentOffline {
		t.Fatalf("err = %v, want ErrAgentOffline", err)
	}
}

func TestSharedTokenStillOpensClientsInCredentialMode(t *testing.T) {
	t.Parallel()
	_, server := newCredentialServer(t, &hookLog{})
	for token, want := range map[string]int{"shared": http.StatusNotFound, "tok-a": http.StatusUnauthorized, "": http.StatusUnauthorized} {
		req, _ := http.NewRequest(http.MethodPost, server.URL+"/client?agent=machine-a", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("token %q: status %d, want %d", token, resp.StatusCode, want)
		}
	}
}

type tapLog struct {
	mu      sync.Mutex
	out     []byte
	resizes [][2]int
	closed  int
}

func (l *tapLog) Output(d []byte) { l.mu.Lock(); l.out = append(l.out, d...); l.mu.Unlock() }
func (l *tapLog) Resize(c, r int) {
	l.mu.Lock()
	l.resizes = append(l.resizes, [2]int{c, r})
	l.mu.Unlock()
}
func (l *tapLog) Close() { l.mu.Lock(); l.closed++; l.mu.Unlock() }

// The tap sees what the agent prints and the browser's resizes, never what the
// browser types, and is closed exactly once when the session ends.
func TestTapSeesOutputAndResizeButNotInput(t *testing.T) {
	t.Parallel()
	relay, server := newCredentialServer(t, &hookLog{})
	agent := registerAs(t, server.URL, "tok-a", protocol.Message{Type: protocol.TypeRegisterAgent})
	waitFor(t, "online", func() bool { return relay.IsOnline("machine-a") })

	tap := &tapLog{}
	adminSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		if _, err := relay.OpenTerminal("machine-a", conn, tap); err != nil {
			t.Errorf("OpenTerminal: %v", err)
		}
	}))
	defer adminSrv.Close()
	browser, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(adminSrv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()

	var start protocol.Message
	_ = agent.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := agent.ReadJSON(&start); err != nil {
		t.Fatal(err)
	}
	session := dialWebSocket(t, server.URL, "/session?id="+url.QueryEscape(start.SessionID), "tok-a")
	for _, conn := range []*websocket.Conn{browser, session} {
		var notice protocol.Message
		if err := conn.ReadJSON(&notice); err != nil {
			t.Fatal(err)
		}
	}

	_ = browser.WriteJSON(protocol.Message{Type: protocol.TypeResize, Cols: 100, Rows: 30})
	_ = browser.WriteMessage(websocket.BinaryMessage, []byte("secret-typed\n"))
	for i := 0; i < 2; i++ { // the agent still receives both the resize and the typed text
		if _, _, err := session.ReadMessage(); err != nil {
			t.Fatal(err)
		}
	}
	_ = session.WriteMessage(websocket.BinaryMessage, []byte("hello\r\n"))
	if _, got, _ := browser.ReadMessage(); string(got) != "hello\r\n" {
		t.Fatalf("browser got %q", got)
	}
	session.Close()

	waitFor(t, "tap closed", func() bool { tap.mu.Lock(); defer tap.mu.Unlock(); return tap.closed == 1 })
	tap.mu.Lock()
	defer tap.mu.Unlock()
	if string(tap.out) != "hello\r\n" {
		t.Errorf("tap output = %q, want only the shell output", tap.out)
	}
	if len(tap.resizes) != 1 || tap.resizes[0] != [2]int{100, 30} {
		t.Errorf("tap resizes = %v", tap.resizes)
	}
}
