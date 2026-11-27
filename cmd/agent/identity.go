package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/user"
	"strconv"
	"strings"
)

// identity is the account that terminals and file transfers run as while the
// agent itself runs as root (--shell-user). It is what login(1) would set up:
// uid, gid, supplementary groups, home and login shell.
type identity struct {
	name   string
	uid    uint32
	gid    uint32
	groups []uint32
	home   string
	shell  string // login shell from the passwd database ("" when unknown)
}

// lookupIdentity resolves an account from the system's user database.
func lookupIdentity(name string) (*identity, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return nil, fmt.Errorf("usuário %q não encontrado nesta máquina", name)
	}
	uid, err1 := strconv.ParseUint(u.Uid, 10, 32)
	gid, err2 := strconv.ParseUint(u.Gid, 10, 32)
	if err1 != nil || err2 != nil {
		return nil, fmt.Errorf("usuário %q tem uid ou gid inválido", name)
	}
	id := &identity{name: u.Username, uid: uint32(uid), gid: uint32(gid), home: u.HomeDir, shell: loginShell(u.Username)}
	if ids, err := u.GroupIds(); err == nil {
		for _, g := range ids {
			if n, err := strconv.ParseUint(g, 10, 32); err == nil {
				id.groups = append(id.groups, uint32(n))
			}
		}
	}
	if len(id.groups) == 0 {
		id.groups = []uint32{id.gid}
	}
	return id, nil
}

// loginShell reads the shell of an account from /etc/passwd.
func loginShell(name string) string {
	f, err := os.Open("/etc/passwd")
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Split(sc.Text(), ":")
		if len(fields) >= 7 && fields[0] == name {
			return fields[6]
		}
	}
	return ""
}

// resolveShellUser validates --shell-user. It returns nil, nil when no switch
// is needed (empty name, or the agent already runs as that account).
func resolveShellUser(name string) (*identity, error) {
	if name == "" {
		return nil, nil
	}
	id, err := lookupIdentity(name)
	if err != nil {
		return nil, err
	}
	if int(id.uid) == os.Geteuid() {
		return nil, nil
	}
	if os.Geteuid() != 0 {
		return nil, errors.New("--shell-user só funciona quando o agent roda como root; do contrário ele não pode trocar de usuário")
	}
	return id, nil
}

// withheldEnv lists what a terminal must not inherit from the agent: its own
// credential and the variables systemd sets for the service. The rest of the
// agent's environment is kept, as before.
func withheldEnv(key string) bool {
	switch key {
	case "INVOCATION_ID", "JOURNAL_STREAM", "SYSTEMD_EXEC_PID", "MAINPID", "MANAGERPID", "NOTIFY_SOCKET",
		"LISTEN_FDS", "LISTEN_PID", "LISTEN_FDNAMES", "WATCHDOG_PID", "WATCHDOG_USEC", "MEMORY_PRESSURE_WATCH", "MEMORY_PRESSURE_WRITE":
		return true
	}
	return strings.HasPrefix(key, "RTERMINAL_")
}

// personalEnv lists what belongs to the root account the agent runs as and
// would be wrong, or worse, in another user's terminal.
func personalEnv(key string) bool {
	switch key {
	case "HOME", "USER", "LOGNAME", "SHELL", "MAIL", "PWD", "OLDPWD", "DBUS_SESSION_BUS_ADDRESS", "SSH_AUTH_SOCK":
		return true
	}
	return strings.HasPrefix(key, "XDG_") || strings.HasPrefix(key, "SUDO_")
}

// sessionEnvironment is the environment of a new terminal: the agent's, minus
// its credential and service variables and, when the terminal runs as another
// account, minus the root-specific ones, with that account's identity filled in.
func sessionEnvironment(environ []string, shellPath string, id *identity) []string {
	kept := make([]string, 0, len(environ)+4)
	for _, kv := range environ {
		key, _, _ := strings.Cut(kv, "=")
		if withheldEnv(key) || (id != nil && personalEnv(key)) {
			continue
		}
		kept = append(kept, kv)
	}
	if id != nil {
		kept = append(kept, "HOME="+id.home, "USER="+id.name, "LOGNAME="+id.name, "SHELL="+shellPath)
	}
	return shellEnvironment(kept, shellPath)
}
