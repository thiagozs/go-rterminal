package admin

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// agentTarget matches os-arch pairs the build ships, e.g. "linux-arm64". It is
// also the only thing ever joined onto the agents directory, so it cannot
// escape it.
var agentTarget = regexp.MustCompile(`^(linux|darwin)-(amd64|arm64|arm)$`)

// agentTargets lists the os-arch binaries present in the configured directory.
func (s *Server) agentTargets() []string {
	if s.agentsDir == "" {
		return nil
	}
	entries, err := os.ReadDir(s.agentsDir)
	if err != nil {
		return nil
	}
	var targets []string
	for _, entry := range entries {
		target, ok := strings.CutPrefix(entry.Name(), "agent-")
		if ok && agentTarget.MatchString(target) && entry.Type().IsRegular() {
			targets = append(targets, target)
		}
	}
	sort.Strings(targets)
	return targets
}

func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	header := r.Header.Get("Authorization")
	if len(header) <= len(prefix) || header[:len(prefix)] != prefix {
		return ""
	}
	return header[len(prefix):]
}

// downloadAgent serves the agent binary for GET /download/agent/{os-arch}. It
// needs a machine credential (what the generated install command sends) or a
// logged-in administrator, since the binary comes from a private project.
func (s *Server) downloadAgent(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	who := ""
	if token := bearerToken(r); token != "" {
		if blocked, _ := s.limiter.blocked("dl:" + ip); blocked {
			writeError(w, http.StatusTooManyRequests, "too many attempts, try again later")
			return
		}
		if machine, err := s.store.MachineByToken(token); err == nil {
			who = "machine:" + machine.ID
		} else {
			s.limiter.fail("dl:" + ip)
			s.logger.Printf("[admin] download do agent recusado (credencial invalida) | ip=%s", ip)
		}
	}
	if who == "" {
		if admin, ok := s.adminFromCookie(r); ok && admin.IsAdmin() && !admin.MustChangePassword {
			who = "admin:" + admin.Username
		}
	}
	if who == "" {
		writeError(w, http.StatusUnauthorized, "machine credential or admin login required")
		return
	}

	target := r.PathValue("target")
	if !agentTarget.MatchString(target) {
		writeError(w, http.StatusNotFound, "unknown target; expected os-arch such as linux-arm64")
		return
	}
	if s.agentsDir == "" {
		writeError(w, http.StatusNotFound, "agent downloads are not enabled on this server")
		return
	}
	file, err := os.Open(filepath.Join(s.agentsDir, "agent-"+target))
	if err != nil {
		writeError(w, http.StatusNotFound, "no agent binary for "+target)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		writeError(w, http.StatusNotFound, "no agent binary for "+target)
		return
	}

	s.audit(who, "agent_download", target, "")
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", `attachment; filename="agent"`)
	h.Set("Cache-Control", "no-store")
	http.ServeContent(w, r, "agent", info.ModTime(), file)
}
