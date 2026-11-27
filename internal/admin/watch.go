package admin

import (
	"net/http"
	"time"

	"github.com/gorilla/websocket"
	"github.com/thiagozs/go-rterminal/internal/store"
)

const watchPing = 20 * time.Second

// listLiveSessions lists the terminals that are open right now.
func (s *Server) listLiveSessions(w http.ResponseWriter, _ *http.Request, _ *store.Admin) {
	writeJSON(w, http.StatusOK, s.live.List(nil))
}

// watchMessage is a text frame sent to a watcher; output goes in binary frames.
type watchMessage struct {
	Type        string `json:"type"` // "info", "resize" or "end"
	MachineID   string `json:"machine_id,omitempty"`
	MachineName string `json:"machine_name,omitempty"`
	Username    string `json:"username,omitempty"`
	Cols        int    `json:"cols,omitempty"`
	Rows        int    `json:"rows,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

// watch streams a running terminal to an administrator, read-only: whatever
// the watcher sends is discarded, so it can never reach the shell.
func (s *Server) watch(w http.ResponseWriter, r *http.Request) {
	admin, ok := s.adminFromCookie(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "login required")
		return
	}
	if pending := s.pendingStep(admin); pending != "" {
		writeError(w, http.StatusForbidden, pending)
		return
	}
	if !admin.IsAdmin() {
		writeError(w, http.StatusForbidden, "administrator required")
		return
	}
	watcher, info, ok := s.live.Watch(r.URL.Query().Get("session"))
	if !ok {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		watcher.Stop()
		return
	}
	defer watcher.Stop()
	defer conn.Close()
	s.audit(admin.Username, "watch_start", info.MachineID, "session "+info.ID+" of "+info.Username)
	defer s.audit(admin.Username, "watch_end", info.MachineID, "session "+info.ID)

	// Discard anything the watcher sends; the read also notices a closed tab.
	gone := make(chan struct{})
	conn.SetReadLimit(512)
	go func() {
		defer close(gone)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	send := func(kind int, data []byte) bool {
		_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		return conn.WriteMessage(kind, data) == nil
	}
	sendJSON := func(m watchMessage) bool {
		_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		return conn.WriteJSON(m) == nil
	}
	if !sendJSON(watchMessage{Type: "info", MachineID: info.MachineID, MachineName: info.MachineName, Username: info.Username}) {
		return
	}
	ping := time.NewTicker(watchPing)
	defer ping.Stop()
	for {
		select {
		case <-gone:
			return
		case <-ping.C:
			if conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(time.Second)) != nil {
				return
			}
		case ev, open := <-watcher.Events:
			if !open {
				return
			}
			switch ev.Kind {
			case "output":
				if !send(websocket.BinaryMessage, ev.Data) {
					return
				}
			case "resize":
				if !sendJSON(watchMessage{Type: "resize", Cols: ev.Cols, Rows: ev.Rows}) {
					return
				}
			case "end":
				reason := "session ended"
				if len(ev.Data) > 0 {
					reason = string(ev.Data)
				}
				sendJSON(watchMessage{Type: "end", Reason: reason})
				return
			}
		}
	}
}
