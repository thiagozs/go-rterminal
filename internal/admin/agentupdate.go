package admin

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/thiagozs/go-rterminal/internal/protocol"
	"github.com/thiagozs/go-rterminal/internal/relay"
	"github.com/thiagozs/go-rterminal/internal/store"
)

type updateRequest struct {
	// Force goes ahead although terminals are open on the machine (they end
	// when the agent restarts).
	Force bool `json:"force"`
}

// agentDigest is the SHA-256 of a shipped agent binary, cached until the file changes.
func (s *Server) agentDigest(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	key := fmt.Sprintf("%s|%d|%d", path, info.Size(), info.ModTime().UnixNano())
	s.digestMu.Lock()
	defer s.digestMu.Unlock()
	if sum, ok := s.digests[key]; ok {
		return sum, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	s.digests = map[string]string{key: sum} // one entry per file is enough: drop stale ones
	return sum, nil
}

// updateAgent asks a connected agent to replace itself with the version this
// server ships. The agent downloads the binary, checks its digest, swaps it
// and restarts; the result shows in the machine's history.
func (s *Server) updateAgent(w http.ResponseWriter, r *http.Request, admin *store.Admin) {
	machine, err := s.store.GetMachine(r.PathValue("id"))
	if err != nil {
		s.storeError(w, err)
		return
	}
	var in updateRequest
	if r.ContentLength > 0 && !decode(w, r, &in) {
		return
	}
	conflict := func(msg string) { writeError(w, http.StatusConflict, msg) }
	switch {
	case s.version == "" || s.version == "dev":
		conflict("this server has no build version, so there is nothing to compare the agent with")
		return
	case !s.relay.IsOnline(machine.ID):
		conflict("machine offline")
		return
	case !s.relay.AgentHas(machine.ID, protocol.CapSelfUpdate):
		conflict("this agent is too old to update itself: run the manual update command once")
		return
	case machine.OS != "linux":
		conflict("automatic update is only available for Linux agents")
		return
	case machine.Version == s.version:
		conflict("the agent is already on version " + s.version)
		return
	}
	target := machine.OS + "-" + machine.Arch
	if !agentTarget.MatchString(target) || s.agentsDir == "" {
		conflict("this server has no agent binary for " + target)
		return
	}
	digest, err := s.agentDigest(filepath.Join(s.agentsDir, "agent-"+target))
	if err != nil {
		conflict("this server has no agent binary for " + target)
		return
	}
	if open := s.openSessions(machine.ID); open > 0 && !in.Force {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":    fmt.Sprintf("%d terminal(s) open on this machine would end when the agent restarts", open),
			"sessions": open,
		})
		return
	}
	err = s.relay.SendToAgent(machine.ID, protocol.Message{Type: protocol.TypeUpdateAgent, Version: s.version, SHA256: digest})
	if errors.Is(err, relay.ErrAgentOffline) {
		conflict("machine offline")
		return
	}
	if err != nil {
		s.logger.Printf("[admin] falha ao pedir atualização | machine=%s err=%v", machine.ID, err)
		writeError(w, http.StatusBadGateway, "could not reach the agent")
		return
	}
	s.audit(admin.Username, "agent_update", machine.ID, fmt.Sprintf("%s -> %s", machine.Version, s.version))
	writeJSON(w, http.StatusAccepted, map[string]any{"version": s.version, "requested_at": time.Now().UTC()})
}

// openSessions counts the web terminals running on a machine.
func (s *Server) openSessions(machineID string) int {
	n := 0
	for _, info := range s.live.List(nil) {
		if info.MachineID == machineID {
			n++
		}
	}
	return n
}
