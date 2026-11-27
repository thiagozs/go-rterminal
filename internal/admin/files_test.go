package admin

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/thiagozs/go-rterminal/internal/protocol"
)

// fileChannel is both ends of an open file channel: the browser and a fake agent.
type fileChannel struct {
	t       *testing.T
	browser *websocket.Conn
	agent   *websocket.Conn // the agent's end of the session
}

func (e *env) openFileChannel(machineID, credential string) *fileChannel {
	e.t.Helper()
	control := e.connectAgent(credential, protocol.Message{Capabilities: []string{protocol.CapFiles}})
	waitUntil(e.t, "online", func() bool { return e.relay.IsOnline(machineID) })
	browser, _, err := websocket.DefaultDialer.Dial(e.wsURL("/ws/files?machine="+machineID), (&person{e: e, client: e.client}).cookieHeader())
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { browser.Close() })
	var start protocol.Message
	_ = control.SetReadDeadline(time.Now().Add(3 * time.Second))
	if err := control.ReadJSON(&start); err != nil {
		e.t.Fatal(err)
	}
	if start.Type != protocol.TypeStartSession || start.Kind != protocol.KindFiles {
		e.t.Fatalf("the agent was asked for %+v, want a files session", start)
	}
	agent, _, err := websocket.DefaultDialer.Dial(e.wsURL("/session?id="+url.QueryEscape(start.SessionID)),
		http.Header{"Authorization": {"Bearer " + credential}})
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { agent.Close() })
	for _, c := range []*websocket.Conn{browser, agent} {
		var paired protocol.Message
		_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
		if err := c.ReadJSON(&paired); err != nil || paired.Type != protocol.TypePaired {
			e.t.Fatalf("pairing: %v %+v", err, paired)
		}
	}
	return &fileChannel{t: e.t, browser: browser, agent: agent}
}

func (c *fileChannel) read(conn *websocket.Conn) (int, []byte) {
	c.t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	kind, data, err := conn.ReadMessage()
	if err != nil {
		c.t.Fatalf("read: %v", err)
	}
	return kind, data
}

func (c *fileChannel) readOp(conn *websocket.Conn) protocol.FileMessage {
	c.t.Helper()
	kind, data := c.read(conn)
	var m protocol.FileMessage
	if kind != websocket.TextMessage || json.Unmarshal(data, &m) != nil {
		c.t.Fatalf("expected a file protocol frame, got %d %q", kind, data)
	}
	return m
}

func (e *env) auditContains(t *testing.T, parts ...string) {
	t.Helper()
	waitUntil(t, "audit "+strings.Join(parts, " "), func() bool {
		_, body := e.do("GET", "/api/audit", nil)
		return containsAll(body, parts...)
	})
}

func TestFileTransferIsAuditedWithRealByteCounts(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.login()
	m := e.createMachine("pi")
	ch := e.openFileChannel("pi", m.Credential)

	// Listing passes through untouched in both directions.
	_ = ch.browser.WriteJSON(protocol.FileMessage{Op: protocol.OpList, Path: "/srv"})
	if got := ch.readOp(ch.agent); got.Op != protocol.OpList || got.Path != "/srv" {
		t.Fatalf("agent saw %+v", got)
	}
	_ = ch.agent.WriteJSON(protocol.FileMessage{Op: protocol.OpList, Path: "/srv", Entries: []protocol.FileEntry{{Name: "a"}}})
	if got := ch.readOp(ch.browser); len(got.Entries) != 1 {
		t.Fatalf("browser saw %+v", got)
	}

	// Upload: the audit records the bytes that really crossed the server.
	_ = ch.browser.WriteJSON(protocol.FileMessage{Op: protocol.OpPut, Path: "/srv/up.bin", Size: 5})
	ch.readOp(ch.agent)
	_ = ch.agent.WriteJSON(protocol.FileMessage{Op: protocol.OpReady})
	ch.readOp(ch.browser)
	_ = ch.browser.WriteMessage(websocket.BinaryMessage, []byte("hello"))
	if kind, data := ch.read(ch.agent); kind != websocket.BinaryMessage || string(data) != "hello" {
		t.Fatalf("agent got %d %q", kind, data)
	}
	_ = ch.browser.WriteJSON(protocol.FileMessage{Op: protocol.OpEnd})
	ch.readOp(ch.agent)
	_ = ch.agent.WriteJSON(protocol.FileMessage{Op: protocol.OpDone, Bytes: 999999}) // the agent's claim is ignored
	ch.readOp(ch.browser)
	e.auditContains(t, "file_upload", "/srv/up.bin (5 bytes)")

	// Download.
	_ = ch.browser.WriteJSON(protocol.FileMessage{Op: protocol.OpGet, Path: "/etc/hosts"})
	ch.readOp(ch.agent)
	_ = ch.agent.WriteJSON(protocol.FileMessage{Op: protocol.OpGet, Path: "/etc/hosts", Size: 3})
	_ = ch.agent.WriteMessage(websocket.BinaryMessage, []byte("abc"))
	_ = ch.agent.WriteJSON(protocol.FileMessage{Op: protocol.OpDone, Bytes: 3})
	ch.readOp(ch.browser)
	ch.read(ch.browser)
	ch.readOp(ch.browser)
	e.auditContains(t, "file_download", "/etc/hosts (3 bytes)")

	// A refused operation is audited as failed, not as done.
	_ = ch.browser.WriteJSON(protocol.FileMessage{Op: protocol.OpGet, Path: "/root/secret"})
	ch.readOp(ch.agent)
	_ = ch.agent.WriteJSON(protocol.FileMessage{Op: protocol.OpError, Error: "sem permissão"})
	ch.readOp(ch.browser)
	e.auditContains(t, "file_failed", "/root/secret", "sem permissão")
}

func TestFileTransferLimitStopsTheChannel(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(c *Config) { c.MaxTransferBytes = 1 << 20 })
	e.login()
	m := e.createMachine("pi")

	t.Run("announced size", func(t *testing.T) {
		ch := e.openFileChannel("pi", m.Credential)
		_ = ch.browser.WriteJSON(protocol.FileMessage{Op: protocol.OpPut, Path: "/tmp/huge", Size: 5 << 20})
		_ = ch.browser.SetReadDeadline(time.Now().Add(3 * time.Second))
		_, _, err := ch.browser.ReadMessage()
		var closeErr *websocket.CloseError
		if !asCloseError(err, &closeErr) || !strings.Contains(closeErr.Text, "limit of 1 MB") {
			t.Fatalf("browser error = %v, want a close that names the limit", err)
		}
		e.auditContains(t, "file_failed", "/tmp/huge", "larger than the limit")
	})
}

func TestFileTransferLimitCountsRealBytes(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(c *Config) { c.MaxTransferBytes = 1 << 20 })
	e.login()
	m := e.createMachine("pi")
	ch := e.openFileChannel("pi", m.Credential)
	// Announces little, sends a lot: the byte count is what stops it.
	_ = ch.browser.WriteJSON(protocol.FileMessage{Op: protocol.OpPut, Path: "/tmp/liar", Size: 10})
	ch.readOp(ch.agent)
	chunk := make([]byte, 256<<10)
	stopped := false
	for i := 0; i < 20 && !stopped; i++ {
		if ch.browser.WriteMessage(websocket.BinaryMessage, chunk) != nil {
			stopped = true
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = ch.browser.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, err := ch.browser.ReadMessage()
	if err == nil {
		t.Fatal("the channel stayed open past the limit")
	}
	e.auditContains(t, "file_failed", "/tmp/liar", "larger than the limit")
}

func asCloseError(err error, target **websocket.CloseError) bool {
	ce, ok := err.(*websocket.CloseError)
	if ok {
		*target = ce
	}
	return ok
}

func TestFileTransferCutOffIsAudited(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.login()
	m := e.createMachine("pi")
	ch := e.openFileChannel("pi", m.Credential)
	_ = ch.browser.WriteJSON(protocol.FileMessage{Op: protocol.OpPut, Path: "/tmp/cut", Size: 100})
	ch.readOp(ch.agent)
	_ = ch.browser.WriteMessage(websocket.BinaryMessage, []byte("partial"))
	ch.read(ch.agent)
	ch.browser.Close()
	e.auditContains(t, "file_failed", "/tmp/cut", "after 7 bytes", "interrupted")
}

func TestFileChannelAccessRules(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.login()
	m := e.createMachine("pi")
	admin := (&person{e: e, client: e.client}).cookieHeader()
	dialStatus := func(path string, header http.Header) int {
		_, resp, err := websocket.DefaultDialer.Dial(e.wsURL(path), header)
		if err == nil {
			t.Fatalf("%s unexpectedly upgraded", path)
		}
		if resp == nil {
			t.Fatalf("%s: %v", path, err)
		}
		return resp.StatusCode
	}
	if got := dialStatus("/ws/files?machine=pi", nil); got != http.StatusUnauthorized {
		t.Errorf("anonymous = %d, want 401", got)
	}
	if got := dialStatus("/ws/files?machine=nope", admin); got != http.StatusNotFound {
		t.Errorf("unknown machine = %d, want 404", got)
	}
	if got := dialStatus("/ws/files?machine=pi", admin); got != http.StatusConflict {
		t.Errorf("offline machine = %d, want 409", got)
	}
	// An agent that never advertised the capability must not be asked for files:
	// an old agent would open a shell on the channel instead.
	e.connectAgent(m.Credential, protocol.Message{})
	waitUntil(t, "online", func() bool { return e.relay.IsOnline("pi") })
	if got := dialStatus("/ws/files?machine=pi", admin); got != http.StatusConflict {
		t.Errorf("agent without the capability = %d, want 409", got)
	}
	_, body := e.do("GET", "/api/machines", nil)
	if strings.Contains(string(body), `"can_files":true`) {
		t.Errorf("machine view claims file support: %s", body)
	}
}

func TestFileTransferCanBeDisabled(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(c *Config) { c.DisableFiles = true })
	e.login()
	m := e.createMachine("pi")
	e.connectAgent(m.Credential, protocol.Message{Capabilities: []string{protocol.CapFiles}})
	waitUntil(t, "online", func() bool { return e.relay.IsOnline("pi") })
	_, resp, err := websocket.DefaultDialer.Dial(e.wsURL("/ws/files?machine=pi"), (&person{e: e, client: e.client}).cookieHeader())
	if err == nil || resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("disabled = %v %v, want 404", err, resp)
	}
	_, body := e.do("GET", "/api/me", nil)
	if !strings.Contains(string(body), `"file_transfer":false`) {
		t.Fatalf("me = %s", body)
	}
}
