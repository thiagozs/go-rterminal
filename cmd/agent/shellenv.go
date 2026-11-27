package main

import (
	"os"
	"os/user"
	"runtime"
	"strings"
)

const defaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// shellEnvironment builds the environment for an interactive PTY shell from
// the agent's own. A systemd service starts with almost nothing (no HOME, no
// TERM), which breaks `cd`, `clear`, colours and prompt themes, so the basics
// of a login session are filled in when they are missing. Values the operator
// set explicitly are never overridden.
func shellEnvironment(environ []string, shellPath string) []string {
	env := map[string]string{}
	var order []string
	for _, kv := range environ {
		key, value, ok := strings.Cut(kv, "=")
		if !ok || key == "" {
			continue
		}
		if _, seen := env[key]; !seen {
			order = append(order, key)
		}
		env[key] = value
	}
	set := func(key, value string) {
		if _, seen := env[key]; !seen {
			order = append(order, key)
		}
		env[key] = value
	}
	missing := func(key string) bool { return env[key] == "" }

	// The sessions are driven by xterm.js or a real terminal: both speak xterm.
	if missing("TERM") || env["TERM"] == "dumb" {
		set("TERM", "xterm-256color")
	}
	if missing("COLORTERM") {
		set("COLORTERM", "truecolor")
	}
	// Icons and prompt glyphs need a UTF-8 locale.
	if missing("LC_ALL") && missing("LC_CTYPE") && missing("LANG") {
		if runtime.GOOS == "darwin" {
			set("LANG", "en_US.UTF-8")
		} else {
			set("LANG", "C.UTF-8")
		}
	}
	if missing("PATH") {
		set("PATH", defaultPath)
	}
	if current, err := user.Current(); err == nil {
		if missing("HOME") && current.HomeDir != "" {
			set("HOME", current.HomeDir)
		}
		if missing("USER") {
			set("USER", current.Username)
		}
		if missing("LOGNAME") {
			set("LOGNAME", current.Username)
		}
	}
	if missing("SHELL") {
		set("SHELL", shellPath)
	}

	out := make([]string, 0, len(order))
	for _, key := range order {
		out = append(out, key+"="+env[key])
	}
	return out
}

// startDirectory is where a new shell opens: the user's home when it exists.
func startDirectory(env []string) string {
	for _, kv := range env {
		if home, ok := strings.CutPrefix(kv, "HOME="); ok && home != "" {
			if info, err := os.Stat(home); err == nil && info.IsDir() {
				return home
			}
		}
	}
	return ""
}
