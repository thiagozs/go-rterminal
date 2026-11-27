package main

import (
	"context"
	"os/user"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func envMap(env []string) map[string]string {
	m := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m
}

func TestShellEnvironmentFillsWhatSystemdLeavesOut(t *testing.T) {
	env := envMap(shellEnvironment(nil, "/bin/zsh"))
	current, _ := user.Current()
	for key, want := range map[string]string{
		"TERM": "xterm-256color", "COLORTERM": "truecolor", "SHELL": "/bin/zsh",
		"HOME": current.HomeDir, "USER": current.Username, "LOGNAME": current.Username,
	} {
		if env[key] != want {
			t.Errorf("%s = %q, want %q", key, env[key], want)
		}
	}
	if !strings.Contains(env["LANG"], "UTF-8") {
		t.Errorf("LANG = %q, want a UTF-8 locale", env["LANG"])
	}
	if env["PATH"] == "" {
		t.Error("PATH not set")
	}
}

func TestShellEnvironmentKeepsOperatorChoices(t *testing.T) {
	env := envMap(shellEnvironment([]string{
		"TERM=screen-256color", "HOME=/srv/me", "LC_ALL=pt_BR.UTF-8", "SHELL=/bin/fish", "COLORTERM=24bit", "PATH=/opt/bin",
	}, "/bin/zsh"))
	for key, want := range map[string]string{
		"TERM": "screen-256color", "HOME": "/srv/me", "SHELL": "/bin/fish", "COLORTERM": "24bit", "PATH": "/opt/bin",
	} {
		if env[key] != want {
			t.Errorf("%s = %q, want the operator's %q", key, env[key], want)
		}
	}
	if _, set := env["LANG"]; set {
		t.Error("LANG added although LC_ALL is set")
	}
}

func TestShellEnvironmentReplacesDumbTerminal(t *testing.T) {
	if got := envMap(shellEnvironment([]string{"TERM=dumb"}, "/bin/sh"))["TERM"]; got != "xterm-256color" {
		t.Errorf("TERM = %q, want dumb replaced", got)
	}
}

func TestStartDirectory(t *testing.T) {
	dir := t.TempDir()
	if got := startDirectory([]string{"A=1", "HOME=" + dir}); got != dir {
		t.Errorf("startDirectory = %q, want %q", got, dir)
	}
	if got := startDirectory([]string{"HOME=/definitely/not/here"}); got != "" {
		t.Errorf("startDirectory for a missing home = %q, want empty", got)
	}
	if got := startDirectory(nil); got != "" {
		t.Errorf("startDirectory without HOME = %q, want empty", got)
	}
}

// A shell started by a service with an empty environment must still get a
// usable session: TERM, HOME, UTF-8, and its working directory in $HOME.
func TestShellSessionFromBareEnvironment(t *testing.T) {
	t.Setenv("TERM", "")
	t.Setenv("HOME", "")
	t.Setenv("LANG", "")
	t.Setenv("LC_ALL", "")
	t.Setenv("LC_CTYPE", "")
	done, peer := newSessionPair(t, "echo \"TERM=$TERM HOME=$HOME LANG=$LANG PWD=$(pwd)\"\n")
	var out strings.Builder
	_ = peer.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		_, data, err := peer.ReadMessage()
		if err != nil {
			if !websocket.IsCloseError(err, websocket.CloseNormalClosure) {
				t.Fatalf("read: %v", err)
			}
			break
		}
		out.Write(data)
	}
	_ = context.Background()
	current, _ := user.Current()
	text := out.String()
	for _, want := range []string{"TERM=xterm-256color", "HOME=" + current.HomeDir, "UTF-8", "PWD=" + current.HomeDir} {
		if !strings.Contains(text, want) {
			t.Errorf("shell environment lacks %q:\n%s", want, text)
		}
	}
	if err := <-done; err != nil {
		t.Fatalf("shellSession: %v", err)
	}
}
