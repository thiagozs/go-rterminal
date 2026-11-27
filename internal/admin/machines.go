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

// archDetect maps `uname -m` to the arch names of the shipped binaries.
const archDetect = `case "$(uname -m)" in
  x86_64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  armv7l|armv6l) ARCH=arm ;;
  *) echo "arquitetura nao suportada: $(uname -m)" >&2; exit 1 ;;
esac`

func (s *Server) commands(r *http.Request, machineID, credential string) Commands {
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
	unit := strings.Join([]string{
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
		return fmt.Sprintf(`printf 'Authorization: Bearer %%s\n' "$TOKEN" | curl -fsSL -H @- "%s/download/agent/%s-$ARCH" -o %s`, base, osExpr, out)
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
