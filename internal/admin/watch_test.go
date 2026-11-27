package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/thiagozs/go-rterminal/internal/protocol"
	"github.com/thiagozs/go-rterminal/internal/store"
)

func readWatch(t *testing.T, c *websocket.Conn) (int, []byte) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	kind, data, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("watch read: %v", err)
	}
	return kind, data
}

func TestWatchLiveSession(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.login()
	m := e.createMachine("raspberry")

	agent := e.connectAgent(m.Credential, protocol.Message{})
	waitUntil(t, "online", func() bool { return e.relay.IsOnline("raspberry") })
	browser, _, err := websocket.DefaultDialer.Dial(e.wsURL("/ws/terminal?machine=raspberry"), (&person{e: e, client: e.client}).cookieHeader())
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	var start protocol.Message
	_ = agent.SetReadDeadline(time.Now().Add(3 * time.Second))
	if err := agent.ReadJSON(&start); err != nil {
		t.Fatal(err)
	}
	shell, _, err := websocket.DefaultDialer.Dial(e.wsURL("/session?id="+url.QueryEscape(start.SessionID)),
		http.Header{"Authorization": {"Bearer " + m.Credential}})
	if err != nil {
		t.Fatal(err)
	}
	defer shell.Close()
	for _, c := range []*websocket.Conn{browser, shell} {
		var notice protocol.Message
		if err := c.ReadJSON(&notice); err != nil {
			t.Fatal(err)
		}
	}
	_ = browser.WriteJSON(protocol.Message{Type: protocol.TypeResize, Cols: 100, Rows: 30})
	_ = shell.WriteMessage(websocket.BinaryMessage, []byte("antes-de-entrar\r\n"))

	var sessions []map[string]any
	waitUntil(t, "session listed", func() bool {
		_, body := e.do("GET", "/api/sessions", nil)
		sessions = nil
		_ = json.Unmarshal(body, &sessions)
		return len(sessions) == 1
	})
	if sessions[0]["machine_name"] != "raspberry" || sessions[0]["username"] != "root" || sessions[0]["id"] != start.SessionID {
		t.Fatalf("sessions = %v", sessions)
	}

	// A watcher gets the size and the recent output, then the live stream.
	admin := &person{e: e, client: e.client}
	watcher, _, err := websocket.DefaultDialer.Dial(e.wsURL("/ws/watch?session="+start.SessionID), admin.cookieHeader())
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close()
	var info watchMessage
	kind, data := readWatch(t, watcher)
	_ = json.Unmarshal(data, &info)
	if kind != websocket.TextMessage || info.Type != "info" || info.MachineName != "raspberry" {
		t.Fatalf("first frame = %d %s", kind, data)
	}
	var resize watchMessage
	_, data = readWatch(t, watcher)
	_ = json.Unmarshal(data, &resize)
	if resize.Type != "resize" || resize.Cols != 100 || resize.Rows != 30 {
		t.Fatalf("resize = %s", data)
	}
	kind, data = readWatch(t, watcher)
	if kind != websocket.BinaryMessage || !bytes.Contains(data, []byte("antes-de-entrar")) {
		t.Fatalf("replay = %d %q", kind, data)
	}
	_ = shell.WriteMessage(websocket.BinaryMessage, []byte("ao-vivo\r\n"))
	_, data = readWatch(t, watcher)
	if !bytes.Contains(data, []byte("ao-vivo")) {
		t.Fatalf("live = %q", data)
	}

	// Read-only: what the watcher sends never reaches the shell.
	_ = watcher.WriteMessage(websocket.BinaryMessage, []byte("rm -rf /\n"))
	_ = watcher.WriteJSON(protocol.Message{Type: protocol.TypeResize, Cols: 5, Rows: 5})
	_ = browser.WriteMessage(websocket.BinaryMessage, []byte("marcador"))
	_ = shell.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		_, got, err := shell.ReadMessage()
		if err != nil {
			t.Fatalf("shell read: %v", err)
		}
		if bytes.Contains(got, []byte("rm -rf")) {
			t.Fatalf("the watcher's input reached the shell: %q", got)
		}
		if bytes.Contains(got, []byte("marcador")) {
			break
		}
	}

	// The end of the session is announced.
	shell.Close()
	for {
		kind, data = readWatch(t, watcher)
		if kind == websocket.TextMessage {
			var end watchMessage
			_ = json.Unmarshal(data, &end)
			if end.Type == "end" {
				break
			}
		}
	}
	waitUntil(t, "session gone", func() bool {
		_, body := e.do("GET", "/api/sessions", nil)
		return bytes.Equal(bytes.TrimSpace(body), []byte("[]"))
	})
	_, audit := e.do("GET", "/api/audit", nil)
	if !bytes.Contains(audit, []byte("watch_start")) {
		t.Errorf("audit lacks watch_start: %s", audit)
	}
	if _, _, err := websocket.DefaultDialer.Dial(e.wsURL("/ws/watch?session="+start.SessionID), admin.cookieHeader()); err == nil {
		t.Error("watching an ended session must fail")
	}
}

func TestWatchIsForAdministrators(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.login()
	code, body := e.do("POST", "/api/users", userInput{Username: "ana", Role: store.RoleOperator})
	if code != http.StatusCreated {
		t.Fatalf("create user = %d", code)
	}
	var created userCreated
	_ = json.Unmarshal(body, &created)
	ana := e.newPerson()
	ana.do("POST", "/api/login", credentials{"ana", created.TemporaryPassword})
	ana.do("POST", "/api/password", passwordInput{Current: created.TemporaryPassword, New: "operador-123"})

	admin := &person{e: e, client: e.client}
	for _, tc := range []struct {
		name   string
		header http.Header
		want   int
	}{
		{"anonymous", nil, http.StatusUnauthorized},
		{"operator", ana.cookieHeader(), http.StatusForbidden},
		{"unknown session", admin.cookieHeader(), http.StatusNotFound},
	} {
		_, resp, err := websocket.DefaultDialer.Dial(e.wsURL("/ws/watch?session=nope"), tc.header)
		if err == nil || resp == nil || resp.StatusCode != tc.want {
			t.Errorf("%s: err=%v resp=%v, want %d", tc.name, err, resp, tc.want)
		}
	}
	if code, _ := ana.do("GET", "/api/sessions", nil); code != http.StatusForbidden {
		t.Errorf("operator sessions = %d, want 403", code)
	}
}
