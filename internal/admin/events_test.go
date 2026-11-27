package admin

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/thiagozs/go-rterminal/internal/protocol"
	"github.com/thiagozs/go-rterminal/internal/store"
)

func TestMachineConnectionHistory(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.login()
	m := e.createMachine("galaxybook")

	agent := e.connectAgent(m.Credential, protocol.Message{Version: "v1"})
	_ = agent.WriteJSON(protocol.Message{Type: protocol.TypeDiagnostics, Events: []protocol.Event{
		{At: time.Now().Add(-10 * time.Minute), Kind: "suspend", Detail: "dormiu 9m"},
		{At: time.Now().Add(-9 * time.Minute), Kind: "lost", Detail: "rede caiu"},
	}})
	// Garbage and unrelated messages are ignored.
	_ = agent.WriteJSON(protocol.Message{Type: "whatever"})
	_ = agent.WriteMessage(websocket.TextMessage, []byte("not json"))

	var events []store.MachineEvent
	waitUntil(t, "events stored", func() bool {
		_, body := e.do("GET", "/api/machines/galaxybook/events", nil)
		events = nil
		_ = json.Unmarshal(body, &events)
		return len(events) >= 3
	})
	kinds := map[string]string{}
	for _, ev := range events {
		kinds[ev.Source+"/"+ev.Kind] = ev.Detail
	}
	if kinds["agent/suspend"] != "dormiu 9m" || kinds["agent/lost"] != "rede caiu" {
		t.Fatalf("agent events = %v", kinds)
	}
	if _, ok := kinds["server/online"]; !ok {
		t.Fatalf("server did not record the connection: %v", kinds)
	}
	for i := 1; i < len(events); i++ {
		if events[i].At.After(events[i-1].At) {
			t.Fatalf("events are not newest first: %+v", events)
		}
	}

	// The server records why the agent went away.
	agent.Close()
	waitUntil(t, "offline recorded", func() bool {
		_, body := e.do("GET", "/api/machines/galaxybook/events", nil)
		events = nil
		_ = json.Unmarshal(body, &events)
		return len(events) > 0 && events[0].Kind == "offline"
	})
	if events[0].Source != "server" || events[0].Detail == "" {
		t.Fatalf("offline event = %+v", events[0])
	}
}

func TestMachineHistoryAccess(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.login()
	e.createMachine("galaxybook")
	if code, _ := e.do("GET", "/api/machines/nope/events", nil); code != http.StatusNotFound {
		t.Errorf("unknown machine = %d, want 404", code)
	}
	anon := e.newPerson()
	if code, _ := anon.do("GET", "/api/machines/galaxybook/events", nil); code != http.StatusUnauthorized {
		t.Errorf("anonymous = %d, want 401", code)
	}
}
