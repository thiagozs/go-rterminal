package admin

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/thiagozs/go-rterminal/internal/store"
)

// Commands are ready-to-run snippets to enrol a machine. They embed the
// machine credential, which is shown only once.
type Commands struct {
	Binary    string `json:"binary"`
	Systemd   string `json:"systemd"`
	GoInstall string `json:"go_install"`
}

type machineView struct {
	store.Machine
	Online bool `json:"online"`
}

type machineCreated struct {
	Machine    machineView `json:"machine"`
	Credential string      `json:"credential"`
	Commands   Commands    `json:"commands"`
}

type machineInput struct {
	Name    string `json:"name"`
	GroupID *int64 `json:"group_id"`
}

func (s *Server) view(m store.Machine) machineView {
	return machineView{Machine: m, Online: s.relay.IsOnline(m.ID)}
}

func (s *Server) listMachines(w http.ResponseWriter, _ *http.Request, _ *store.Admin) {
	machines, err := s.store.ListMachines()
	if err != nil {
		s.storeError(w, err)
		return
	}
	views := make([]machineView, 0, len(machines))
	for _, m := range machines {
		views = append(views, s.view(m))
	}
	writeJSON(w, http.StatusOK, views)
}

func (s *Server) createMachine(w http.ResponseWriter, r *http.Request, admin *store.Admin) {
	var in machineInput
	if !decode(w, r, &in) {
		return
	}
	machine, credential, err := s.store.CreateMachine(in.Name, in.GroupID)
	if err != nil {
		s.storeError(w, err)
		return
	}
	s.audit(admin.Username, "machine_create", machine.ID, "")
	writeJSON(w, http.StatusCreated, machineCreated{
		Machine: s.view(*machine), Credential: credential, Commands: s.commands(r, machine.ID, credential),
	})
}

func (s *Server) updateMachine(w http.ResponseWriter, r *http.Request, admin *store.Admin) {
	id := r.PathValue("id")
	var in machineInput
	if !decode(w, r, &in) {
		return
	}
	if err := s.store.UpdateMachine(id, in.Name, in.GroupID); err != nil {
		s.storeError(w, err)
		return
	}
	machine, err := s.store.GetMachine(id)
	if err != nil {
		s.storeError(w, err)
		return
	}
	s.audit(admin.Username, "machine_update", id, "")
	writeJSON(w, http.StatusOK, s.view(*machine))
}

func (s *Server) deleteMachine(w http.ResponseWriter, r *http.Request, admin *store.Admin) {
	id := r.PathValue("id")
	if err := s.store.DeleteMachine(id); err != nil {
		s.storeError(w, err)
		return
	}
	s.relay.Disconnect(id, "machine removed")
	s.audit(admin.Username, "machine_delete", id, "")
	w.WriteHeader(http.StatusNoContent)
}

// rotateMachine issues a new credential and kicks the connected agent, which
// can no longer reconnect with the old one.
func (s *Server) rotateMachine(w http.ResponseWriter, r *http.Request, admin *store.Admin) {
	id := r.PathValue("id")
	credential, err := s.store.RotateMachineToken(id)
	if err != nil {
		s.storeError(w, err)
		return
	}
	machine, err := s.store.GetMachine(id)
	if err != nil {
		s.storeError(w, err)
		return
	}
	s.relay.Disconnect(id, "credential rotated")
	s.audit(admin.Username, "machine_rotate", id, "")
	writeJSON(w, http.StatusOK, machineCreated{
		Machine: s.view(*machine), Credential: credential, Commands: s.commands(r, id, credential),
	})
}

// baseURL is the externally visible server URL, from config or the request.
func (s *Server) baseURL(r *http.Request) string {
	if s.publicURL != "" {
		return s.publicURL
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (s *Server) commands(r *http.Request, machineID, credential string) Commands {
	server := s.baseURL(r)
	if u, err := url.Parse(server); err == nil {
		switch u.Scheme {
		case "https":
			u.Scheme = "wss"
		case "http":
			u.Scheme = "ws"
		}
		server = u.String()
	}
	args := fmt.Sprintf("--server-url %s --agent-id %s", server, machineID)
	return Commands{
		GoInstall: "go install github.com/thiagozs/go-rterminal/cmd/agent@latest",
		Binary:    fmt.Sprintf("RTERMINAL_TOKEN=%s agent %s", credential, args),
		Systemd: strings.Join([]string{
			"# Guarda a credencial num arquivo legivel so pelo root e cria o servico",
			"sudo install -m 600 /dev/null /etc/rterminal-agent.env",
			fmt.Sprintf("echo 'RTERMINAL_TOKEN=%s' | sudo tee /etc/rterminal-agent.env >/dev/null", credential),
			"sudo tee /etc/systemd/system/rterminal-agent.service >/dev/null <<'EOF'",
			"[Unit]",
			"Description=go-rterminal agent",
			"After=network-online.target",
			"Wants=network-online.target",
			"",
			"[Service]",
			"EnvironmentFile=/etc/rterminal-agent.env",
			"ExecStart=/usr/local/bin/agent " + args,
			"Restart=always",
			"RestartSec=5",
			"",
			"[Install]",
			"WantedBy=multi-user.target",
			"EOF",
			"sudo systemctl daemon-reload && sudo systemctl enable --now rterminal-agent",
		}, "\n"),
	}
}
