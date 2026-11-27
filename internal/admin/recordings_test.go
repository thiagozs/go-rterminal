package admin

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/thiagozs/go-rterminal/internal/protocol"
	"github.com/thiagozs/go-rterminal/internal/recording"
	"github.com/thiagozs/go-rterminal/internal/store"
)

// runSession opens a web terminal on machine, has the "shell" print output and
// the "browser" type input, then closes it.
func (e *env) runSession(machineID, credential string, p *person, output string) {
	e.t.Helper()
	agent := e.connectAgent(credential, protocol.Message{})
	waitUntil(e.t, "online", func() bool { return e.relay.IsOnline(machineID) })
	browser, _, err := websocket.DefaultDialer.Dial(e.wsURL("/ws/terminal?machine="+machineID), p.cookieHeader())
	if err != nil {
		e.t.Fatal(err)
	}
	defer browser.Close()
	var start protocol.Message
	_ = agent.SetReadDeadline(time.Now().Add(3 * time.Second))
	if err := agent.ReadJSON(&start); err != nil {
		e.t.Fatal(err)
	}
	session, _, err := websocket.DefaultDialer.Dial(e.wsURL("/session?id="+url.QueryEscape(start.SessionID)),
		http.Header{"Authorization": {"Bearer " + credential}})
	if err != nil {
		e.t.Fatal(err)
	}
	for _, c := range []*websocket.Conn{browser, session} {
		var notice protocol.Message
		if err := c.ReadJSON(&notice); err != nil {
			e.t.Fatal(err)
		}
	}
	_ = browser.WriteJSON(protocol.Message{Type: protocol.TypeResize, Cols: 132, Rows: 43})
	_ = browser.WriteMessage(websocket.BinaryMessage, []byte("my-password-typed-blind\n"))
	for i := 0; i < 2; i++ {
		_, _, _ = session.ReadMessage()
	}
	_ = session.WriteMessage(websocket.BinaryMessage, []byte(output))
	_, _, _ = browser.ReadMessage()
	session.Close()
}

func TestRecordingLifecycle(t *testing.T) {
	t.Parallel()
	var manager *recording.Manager
	e := newEnvWithStore(t, func(c *Config, st *store.Store) {
		m, err := recording.NewManager(filepath.Join(t.TempDir(), "rec"), st, 0, log.New(io.Discard, "", 0))
		if err != nil {
			t.Fatal(err)
		}
		manager = m
		c.Recordings = m
	})
	e.login()
	m := e.createMachine("raspberry")
	admin := &person{e: e, client: e.client}

	e.runSession("raspberry", m.Credential, admin, "linha-1\r\nação → ok\r\n")

	var list []recordingView
	waitUntil(t, "recording finished", func() bool {
		_, body := e.do("GET", "/api/recordings", nil)
		list = nil
		_ = json.Unmarshal(body, &list)
		return len(list) == 1 && list[0].EndedAt != nil
	})
	rec := list[0]
	if rec.Username != "root" || rec.MachineName != "raspberry" || rec.DurationSeconds == nil || rec.Bytes == 0 {
		t.Fatalf("recording = %+v", rec)
	}

	code, cast := e.do("GET", "/api/recordings/"+rec.ID+"/cast", nil)
	if code != 200 {
		t.Fatalf("cast = %d", code)
	}
	lines := strings.Split(strings.TrimSpace(string(cast)), "\n")
	var header map[string]any
	_ = json.Unmarshal([]byte(lines[0]), &header)
	if header["version"] != float64(2) || header["width"] != float64(132) || header["height"] != float64(43) {
		t.Fatalf("header = %v", header)
	}
	if !bytes.Contains(cast, []byte("linha-1")) || !bytes.Contains(cast, []byte("ação → ok")) {
		t.Fatalf("cast lacks the shell output: %s", cast)
	}
	if bytes.Contains(cast, []byte("my-password-typed-blind")) {
		t.Fatalf("what the user typed must never be recorded: %s", cast)
	}

	_, audit := e.do("GET", "/api/audit", nil)
	for _, want := range []string{"terminal_open", "recording " + rec.ID, "recording_view"} {
		if !bytes.Contains(audit, []byte(want)) {
			t.Errorf("audit lacks %q: %s", want, audit)
		}
	}

	// Operators cannot list, watch or delete.
	groupCode, gbody := e.do("POST", "/api/groups", groupInput{Name: "g"})
	if groupCode != http.StatusCreated {
		t.Fatal(string(gbody))
	}
	code, body := e.do("POST", "/api/users", userInput{Username: "ana", Role: store.RoleOperator})
	var created userCreated
	_ = json.Unmarshal(body, &created)
	ana := e.newPerson()
	ana.do("POST", "/api/login", credentials{"ana", created.TemporaryPassword})
	ana.do("POST", "/api/password", passwordInput{Current: created.TemporaryPassword, New: "operador-123"})
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/recordings"}, {"GET", "/api/recordings/" + rec.ID + "/cast"}, {"DELETE", "/api/recordings/" + rec.ID},
	} {
		if code, _ := ana.do(c.method, c.path, nil); code != http.StatusForbidden {
			t.Errorf("operator %s %s = %d, want 403", c.method, c.path, code)
		}
	}

	// A recording outlives its machine, then can be deleted.
	if code, _ := e.do("DELETE", "/api/machines/raspberry", nil); code != http.StatusNoContent {
		t.Fatalf("delete machine = %d", code)
	}
	if code, _ := e.do("GET", "/api/recordings/"+rec.ID+"/cast", nil); code != 200 {
		t.Fatalf("cast after machine delete = %d", code)
	}
	if code, _ := e.do("GET", "/api/recordings/not-a-uuid/cast", nil); code != http.StatusNotFound {
		t.Errorf("bad id = %d, want 404", code)
	}
	if code, _ := e.do("DELETE", "/api/recordings/"+rec.ID, nil); code != http.StatusNoContent {
		t.Fatalf("delete recording = %d", code)
	}
	if code, _ := e.do("GET", "/api/recordings/"+rec.ID+"/cast", nil); code != http.StatusNotFound {
		t.Fatalf("cast after delete = %d", code)
	}
	if f, err := manager.Open(rec.ID); err == nil {
		f.Close()
		t.Fatal("file still on disk")
	}
}

func TestRecordingsDisabled(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.login()
	if code, _ := e.do("GET", "/api/recordings", nil); code != http.StatusNotFound {
		t.Fatalf("recordings off = %d, want 404", code)
	}
	_, body := e.do("GET", "/api/me", nil)
	if !bytes.Contains(body, []byte(`"recording":false`)) {
		t.Fatalf("me = %s", body)
	}
}
