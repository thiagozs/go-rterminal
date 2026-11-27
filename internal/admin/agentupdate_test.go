package admin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/thiagozs/go-rterminal/internal/protocol"
)

func updateEnvWithAgents(t *testing.T) (*env, string) {
	t.Helper()
	dir := t.TempDir()
	payload := []byte("#!/bin/sh\necho agent v2\n")
	if err := os.WriteFile(filepath.Join(dir, "agent-linux-amd64"), payload, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	return newEnv(t, func(c *Config) { c.AgentsDir = dir; c.Version = "v2" }), hex.EncodeToString(sum[:])
}

func (e *env) connectUpdatable(credential, version string, caps ...string) *websocket.Conn {
	return e.connectAgent(credential, protocol.Message{OS: "linux", Arch: "amd64", Version: version, Capabilities: caps})
}

func TestUpdateAgentSendsVerifiedRequest(t *testing.T) {
	t.Parallel()
	e, digest := updateEnvWithAgents(t)
	e.login()
	m := e.createMachine("pi")
	agent := e.connectUpdatable(m.Credential, "v1", protocol.CapSelfUpdate)
	waitUntil(t, "online", func() bool { return e.relay.IsOnline("pi") })
	waitUntil(t, "version stored", func() bool {
		_, body := e.do("GET", "/api/machines", nil)
		var list []machineView
		_ = json.Unmarshal(body, &list)
		return len(list) == 1 && list[0].Version == "v1" && list[0].CanSelfUpdate
	})

	code, body := e.do("POST", "/api/machines/pi/update", nil)
	if code != http.StatusAccepted {
		t.Fatalf("update = %d %s", code, body)
	}
	var msg protocol.Message
	_ = agent.SetReadDeadline(time.Now().Add(3 * time.Second))
	if err := agent.ReadJSON(&msg); err != nil {
		t.Fatal(err)
	}
	if msg.Type != protocol.TypeUpdateAgent || msg.Version != "v2" || msg.SHA256 != digest {
		t.Fatalf("message = %+v, want digest %s", msg, digest)
	}
	_, audit := e.do("GET", "/api/audit", nil)
	if !containsAll(audit, "agent_update", `v1 -\u003e v2`) {
		t.Fatalf("audit = %s", audit)
	}
}

func containsAll(b []byte, parts ...string) bool {
	for _, p := range parts {
		if !strings.Contains(string(b), p) {
			return false
		}
	}
	return true
}

func TestUpdateAgentRefusals(t *testing.T) {
	t.Parallel()
	e, _ := updateEnvWithAgents(t)
	e.login()
	if code, _ := e.do("POST", "/api/machines/nope/update", nil); code != http.StatusNotFound {
		t.Errorf("unknown machine = %d, want 404", code)
	}
	cases := []struct {
		name    string
		machine string
		connect func(cred string) *websocket.Conn
	}{
		{"offline", "off", nil},
		{"old agent without the capability", "old", func(c string) *websocket.Conn { return e.connectUpdatable(c, "v1") }},
		{"already current", "cur", func(c string) *websocket.Conn { return e.connectUpdatable(c, "v2", protocol.CapSelfUpdate) }},
		{"not linux", "mac", func(c string) *websocket.Conn {
			return e.connectAgent(c, protocol.Message{OS: "darwin", Arch: "arm64", Version: "v1", Capabilities: []string{protocol.CapSelfUpdate}})
		}},
		{"no binary for the architecture", "arm", func(c string) *websocket.Conn {
			return e.connectAgent(c, protocol.Message{OS: "linux", Arch: "arm64", Version: "v1", Capabilities: []string{protocol.CapSelfUpdate}})
		}},
	}
	for _, c := range cases {
		m := e.createMachine(c.machine)
		var conn *websocket.Conn
		if c.connect != nil {
			conn = c.connect(m.Credential)
			waitUntil(t, c.name+" online", func() bool { return e.relay.IsOnline(c.machine) })
			waitUntil(t, c.name+" registered", func() bool {
				mm, err := e.store.GetMachine(c.machine)
				return err == nil && mm.Version != ""
			})
		}
		code, body := e.do("POST", "/api/machines/"+c.machine+"/update", nil)
		if code != http.StatusConflict {
			t.Errorf("%s: status = %d %s, want 409", c.name, code, body)
		}
		if conn != nil {
			_ = conn.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
			var msg protocol.Message
			if err := conn.ReadJSON(&msg); err == nil {
				t.Errorf("%s: the agent received %+v although the request was refused", c.name, msg)
			}
		}
	}
}

func TestUpdateAgentWarnsAboutOpenTerminals(t *testing.T) {
	t.Parallel()
	e, _ := updateEnvWithAgents(t)
	e.login()
	m := e.createMachine("pi")
	agent := e.connectUpdatable(m.Credential, "v1", protocol.CapSelfUpdate)
	waitUntil(t, "online", func() bool { return e.relay.IsOnline("pi") })

	// Open a terminal and make it print, so it counts as running.
	browser, _, err := websocket.DefaultDialer.Dial(e.wsURL("/ws/terminal?machine=pi"), (&person{e: e, client: e.client}).cookieHeader())
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	var start protocol.Message
	_ = agent.SetReadDeadline(time.Now().Add(3 * time.Second))
	if err := agent.ReadJSON(&start); err != nil {
		t.Fatal(err)
	}
	shell, _, err := websocket.DefaultDialer.Dial(e.wsURL("/session?id="+start.SessionID), http.Header{"Authorization": {"Bearer " + m.Credential}})
	if err != nil {
		t.Fatal(err)
	}
	defer shell.Close()
	for _, c := range []*websocket.Conn{browser, shell} {
		var notice protocol.Message
		_ = c.ReadJSON(&notice)
	}
	_ = shell.WriteMessage(websocket.BinaryMessage, []byte("hello"))
	waitUntil(t, "terminal listed", func() bool { return e.relay != nil && e.sessionsOf("pi") == 1 })

	code, body := e.do("POST", "/api/machines/pi/update", nil)
	var warn struct {
		Error    string `json:"error"`
		Sessions int    `json:"sessions"`
	}
	_ = json.Unmarshal(body, &warn)
	if code != http.StatusConflict || warn.Sessions != 1 {
		t.Fatalf("without force = %d %s, want 409 with the session count", code, body)
	}
	code, body = e.do("POST", "/api/machines/pi/update", updateRequest{Force: true})
	if code != http.StatusAccepted {
		t.Fatalf("with force = %d %s", code, body)
	}
}

func (e *env) sessionsOf(machine string) int {
	_, body := e.do("GET", "/api/sessions", nil)
	var list []struct {
		MachineID string `json:"machine_id"`
	}
	_ = json.Unmarshal(body, &list)
	n := 0
	for _, s := range list {
		if s.MachineID == machine {
			n++
		}
	}
	return n
}

func TestUpdateAgentIsAdminOnly(t *testing.T) {
	t.Parallel()
	e, _ := updateEnvWithAgents(t)
	e.login()
	e.createMachine("pi")
	anon := e.newPerson()
	if code, _ := anon.do("POST", "/api/machines/pi/update", nil); code != http.StatusUnauthorized {
		t.Errorf("anonymous = %d, want 401", code)
	}
}

func TestUpdateAgentNeedsBuildVersion(t *testing.T) {
	t.Parallel()
	e := newEnv(t) // no Version configured
	e.login()
	m := e.createMachine("pi")
	e.connectUpdatable(m.Credential, "v1", protocol.CapSelfUpdate)
	waitUntil(t, "online", func() bool { return e.relay.IsOnline("pi") })
	if code, _ := e.do("POST", "/api/machines/pi/update", nil); code != http.StatusConflict {
		t.Fatalf("no server version = %d, want 409", code)
	}
}
