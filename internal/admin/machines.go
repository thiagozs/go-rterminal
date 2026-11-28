package admin

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/thiagozs/go-rterminal/internal/protocol"
	"github.com/thiagozs/go-rterminal/internal/store"
)

// Commands are ready-to-run snippets to enrol a machine. They embed the
// machine credential, which is shown only once.
type Commands struct {
	// Binary runs the agent in the foreground (Linux/macOS).
	Binary string `json:"binary"`
	// Systemd installs the agent as a service (Linux).
	Systemd string `json:"systemd"`
	// Downloads is true when the snippets fetch the agent binary from this server.
	Downloads bool `json:"downloads"`
	// GoInstall is the fallback when the server ships no binaries.
	GoInstall string `json:"go_install,omitempty"`
}

type machineView struct {
	store.Machine
	Online bool `json:"online"`
	// CanSelfUpdate is true when the connected agent can replace itself on
	// request (older agents cannot and need the manual command once).
	CanSelfUpdate bool `json:"can_self_update"`
	// CanFiles is true when the connected agent serves file transfer.
	CanFiles bool `json:"can_files"`
	// CanFilesManage is true when the agent also deletes, renames and zips.
	CanFilesManage bool `json:"can_files_manage"`
	// CanFilesCopy is true when the agent also copies files and folders.
	CanFilesCopy bool `json:"can_files_copy"`
}

type machineCreated struct {
	Machine    machineView `json:"machine"`
	Credential string      `json:"credential"`
	Commands   Commands    `json:"commands"`
}

type machineInput struct {
	Name    string `json:"name"`
	GroupID *int64 `json:"group_id"`
	// ShellUser is the account terminals and file transfers open as. Absent
	// leaves it unchanged; "" clears it.
	ShellUser *string `json:"shell_user"`
}

func (s *Server) view(m store.Machine) machineView {
	online := s.relay.IsOnline(m.ID)
	return machineView{
		Machine: m, Online: online,
		CanSelfUpdate:  online && s.relay.AgentHas(m.ID, protocol.CapSelfUpdate),
		CanFiles:       online && s.files && s.relay.AgentHas(m.ID, protocol.CapFiles),
		CanFilesManage: online && s.files && s.relay.AgentHas(m.ID, protocol.CapFilesManage),
		CanFilesCopy:   online && s.files && s.relay.AgentHas(m.ID, protocol.CapFilesCopy),
	}
}

func (s *Server) listMachines(w http.ResponseWriter, _ *http.Request, admin *store.Admin) {
	machines, err := s.store.ListMachines()
	if err != nil {
		s.storeError(w, err)
		return
	}
	scope, err := s.groupScope(admin)
	if err != nil {
		s.storeError(w, err)
		return
	}
	views := make([]machineView, 0, len(machines))
	for _, m := range machines {
		// Operators only see machines of their groups (never ungrouped ones).
		if scope != nil && (m.GroupID == nil || !scope[*m.GroupID]) {
			continue
		}
		views = append(views, s.view(m))
	}
	writeJSON(w, http.StatusOK, views)
}

func (s *Server) createMachine(w http.ResponseWriter, r *http.Request, admin *store.Admin) {
	var in machineInput
	if !decode(w, r, &in) {
		return
	}
	if in.ShellUser != nil && !store.ValidShellUser(*in.ShellUser) {
		writeError(w, http.StatusBadRequest, "invalid shell user: use a login name such as thiago")
		return
	}
	machine, credential, err := s.store.CreateMachine(in.Name, in.GroupID)
	if err != nil {
		s.storeError(w, err)
		return
	}
	if in.ShellUser != nil && *in.ShellUser != "" {
		if err := s.store.SetMachineShellUser(machine.ID, *in.ShellUser); err != nil {
			s.storeError(w, err)
			return
		}
		machine.ShellUser = strings.TrimSpace(*in.ShellUser)
	}
	s.audit(admin.Username, "machine_create", machine.ID, "")
	writeJSON(w, http.StatusCreated, machineCreated{
		Machine: s.view(*machine), Credential: credential, Commands: s.commands(r, machine.ID, credential, machine.ShellUser),
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
	if in.ShellUser != nil {
		if err := s.store.SetMachineShellUser(id, *in.ShellUser); err != nil {
			s.storeError(w, err)
			return
		}
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
		Machine: s.view(*machine), Credential: credential, Commands: s.commands(r, id, credential, machine.ShellUser),
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

// archDetect maps `uname -m` to the arch names of the shipped binaries.
const archDetect = `case "$(uname -m)" in
  x86_64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  armv7l|armv6l) ARCH=arm ;;
  *) echo "arquitetura nao suportada: $(uname -m)" >&2; exit 1 ;;
esac`

func (s *Server) commands(r *http.Request, machineID, credential, shellUser string) Commands {
	base := s.baseURL(r)
	wsURL := base
	if u, err := url.Parse(base); err == nil {
		switch u.Scheme {
		case "https":
			u.Scheme = "wss"
		case "http":
			u.Scheme = "ws"
		}
		wsURL = u.String()
	}
	args := fmt.Sprintf("--server-url %s --agent-id %s", wsURL, machineID)
	// The service runs as root and opens terminals and file transfers as this
	// account, which is what also lets the agent replace its own binary.
	unitArgs := args
	if shellUser != "" {
		unitArgs += " --shell-user " + shellUser
	}
	unit := strings.Join([]string{
		"sudo tee /etc/systemd/system/rterminal-agent.service >/dev/null <<'EOF'",
		"[Unit]",
		"Description=go-rterminal agent",
		"After=network-online.target",
		"Wants=network-online.target",
		"",
		"[Service]",
		"EnvironmentFile=/etc/rterminal-agent.env",
		"ExecStart=/usr/local/bin/agent " + unitArgs,
		"Restart=always",
		"RestartSec=5",
		"",
		"[Install]",
		"WantedBy=multi-user.target",
		"EOF",
	}, "\n")

	if len(s.agentTargets()) == 0 {
		// The server ships no binaries: assume `agent` is already installed.
		return Commands{
			GoInstall: "go install github.com/thiagozs/go-rterminal/cmd/agent@latest",
			Binary:    fmt.Sprintf("RTERMINAL_TOKEN=%s agent %s", credential, args),
			Systemd: strings.Join([]string{
				"# Guarda a credencial num arquivo legivel so pelo root e cria o servico",
				"sudo install -m 600 /dev/null /etc/rterminal-agent.env",
				fmt.Sprintf("echo 'RTERMINAL_TOKEN=%s' | sudo tee /etc/rterminal-agent.env >/dev/null", credential),
				unit,
				"sudo systemctl daemon-reload && sudo systemctl enable --now rterminal-agent",
			}, "\n"),
		}
	}

	// The token goes to curl through stdin (-H @-), so it never shows up in `ps`.
	fetch := func(osExpr, out string) string {
		return fmt.Sprintf(`rm -f %[3]s; printf 'Authorization: Bearer %%s\n' "$TOKEN" | curl -fsSL --retry 8 --retry-all-errors --retry-delay 2 --connect-timeout 15 --speed-limit 1000 --speed-time 20 -C - -H @- "%[1]s/download/agent/%[2]s-$ARCH" -o %[3]s`, base, osExpr, out)
	}
	return Commands{
		Downloads: true,
		Systemd: strings.Join([]string{
			"# Baixa o agent para esta arquitetura, instala o servico e inicia (Linux)",
			fmt.Sprintf("TOKEN='%s'", credential),
			archDetect,
			fetch("linux", "/tmp/rterminal-agent"),
			"sudo install -m 755 /tmp/rterminal-agent /usr/local/bin/agent.new && sudo mv -f /usr/local/bin/agent.new /usr/local/bin/agent && rm -f /tmp/rterminal-agent",
			"sudo install -m 600 /dev/null /etc/rterminal-agent.env",
			`echo "RTERMINAL_TOKEN=$TOKEN" | sudo tee /etc/rterminal-agent.env >/dev/null`,
			unit,
			"sudo systemctl daemon-reload && sudo systemctl enable rterminal-agent && sudo systemctl restart rterminal-agent",
			"unset TOKEN ARCH",
		}, "\n"),
		Binary: strings.Join([]string{
			"# Baixa o agent para ./agent e roda em primeiro plano (Linux ou macOS)",
			fmt.Sprintf("TOKEN='%s'", credential),
			`OS=$(uname -s | tr '[:upper:]' '[:lower:]')`,
			archDetect,
			fetch("$OS", "agent") + " && chmod +x agent",
			fmt.Sprintf(`RTERMINAL_TOKEN="$TOKEN" ./agent %s`, args),
		}, "\n"),
	}
}

// machineEvents returns the connection history of a machine: what the agent
// noticed (suspension, network loss, ...) and what the server observed.
func (s *Server) machineEvents(w http.ResponseWriter, r *http.Request, _ *store.Admin) {
	machine, err := s.store.GetMachine(r.PathValue("id"))
	if err != nil {
		s.storeError(w, err)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	events, err := s.store.ListMachineEvents(machine.ID, limit)
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, events)
}
