package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/thiagozs/go-rterminal/internal/protocol"
	"github.com/thiagozs/go-rterminal/internal/relay"
)

const defaultMaxTransfer = 512 << 20

// filesChannel opens a file transfer channel to a machine's agent. The
// browser then speaks the file protocol (see protocol/files.go) straight to
// the agent through the relay; this server only checks who may do it, audits
// what is transferred and enforces the size limit.
func (s *Server) filesChannel(w http.ResponseWriter, r *http.Request) {
	admin, ok := s.adminFromCookie(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "login required")
		return
	}
	if pending := s.pendingStep(admin); pending != "" {
		writeError(w, http.StatusForbidden, pending)
		return
	}
	if !s.files {
		writeError(w, http.StatusNotFound, "file transfer is disabled on this server")
		return
	}
	machine, err := s.store.GetMachine(r.URL.Query().Get("machine"))
	if err != nil {
		s.storeError(w, err)
		return
	}
	allowed, err := s.store.CanAccessMachine(admin, machine)
	if err != nil {
		s.storeError(w, err)
		return
	}
	if !allowed {
		s.audit(admin.Username, "file_denied", machine.ID, "outside the account's groups")
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if !s.relay.IsOnline(machine.ID) {
		writeError(w, http.StatusConflict, "machine offline")
		return
	}
	if !s.relay.AgentHas(machine.ID, protocol.CapFiles) {
		writeError(w, http.StatusConflict, "this agent does not support file transfer: update it")
		return
	}
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	mon := &fileMonitor{s: s, actor: admin.Username, machineID: machine.ID, limit: s.maxFile}
	sessionID, err := s.relay.OpenChannel(machine.ID, protocol.KindFiles, conn, mon)
	if err != nil {
		reason := "could not open the file channel"
		if errors.Is(err, relay.ErrAgentOffline) {
			reason = "machine offline"
		}
		_ = conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseTryAgainLater, reason), time.Now().Add(time.Second))
		_ = conn.Close()
		return
	}
	s.audit(admin.Username, "file_channel", machine.ID, "session "+sessionID)
}

// fileMonitor audits the transfers of one channel and stops any that exceed
// the size limit. Byte counts are what actually crossed the server, not what
// either end claims.
type fileMonitor struct {
	s         *Server
	actor     string
	machineID string
	limit     int64

	mu sync.Mutex
	op *fileOp
}

type fileOp struct {
	kind  string // protocol.OpPut, OpGet or OpMkdir
	path  string
	bytes int64
}

func (m *fileMonitor) Message(fromAgent bool, messageType int, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if messageType == websocket.BinaryMessage {
		if m.op != nil {
			m.op.bytes += int64(len(data))
			if m.limit > 0 && m.op.bytes > m.limit {
				m.fail("larger than the limit")
				return fmt.Errorf("file larger than the limit of %d MB", m.limit>>20)
			}
		}
		return nil
	}
	var msg protocol.FileMessage
	if json.Unmarshal(data, &msg) != nil {
		return nil
	}
	if !fromAgent {
		switch msg.Op {
		case protocol.OpPut, protocol.OpGet, protocol.OpMkdir:
			if m.op != nil {
				m.fail("replaced by another operation")
			}
			m.op = &fileOp{kind: msg.Op, path: msg.Path}
			if msg.Op == protocol.OpPut && m.limit > 0 && msg.Size > m.limit {
				m.fail("larger than the limit")
				return fmt.Errorf("file larger than the limit of %d MB", m.limit>>20)
			}
		}
		return nil
	}
	switch msg.Op {
	case protocol.OpGet:
		if m.op != nil && m.limit > 0 && msg.Size > m.limit {
			m.fail("larger than the limit")
			return fmt.Errorf("file larger than the limit of %d MB", m.limit>>20)
		}
	case protocol.OpDone:
		if m.op == nil {
			return nil
		}
		action := map[string]string{protocol.OpPut: "file_upload", protocol.OpGet: "file_download", protocol.OpMkdir: "file_mkdir"}[m.op.kind]
		detail := m.op.path
		if m.op.kind != protocol.OpMkdir {
			detail = fmt.Sprintf("%s (%d bytes)", m.op.path, m.op.bytes)
		}
		m.s.audit(m.actor, action, m.machineID, detail)
		m.op = nil
	case protocol.OpError:
		if m.op != nil {
			m.fail(msg.Error)
		}
	}
	return nil
}

// fail audits the current operation as not completed. m.mu must be held.
func (m *fileMonitor) fail(why string) {
	if m.op == nil {
		return
	}
	m.s.audit(m.actor, "file_failed", m.machineID, fmt.Sprintf("%s %s after %d bytes: %s", m.op.kind, m.op.path, m.op.bytes, why))
	m.op = nil
}

// Close notes a transfer that was cut off by the end of the channel.
func (m *fileMonitor) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fail("interrupted")
}
