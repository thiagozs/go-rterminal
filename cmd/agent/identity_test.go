package main

import (
	"archive/zip"
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/thiagozs/go-rterminal/internal/protocol"
)

func TestResolveShellUser(t *testing.T) {
	if id, err := resolveShellUser(""); id != nil || err != nil {
		t.Fatalf("empty = %v %v, want nothing to do", id, err)
	}
	current, err := user.Current()
	if err != nil {
		t.Skip(err)
	}
	if id, err := resolveShellUser(current.Username); id != nil || err != nil {
		t.Fatalf("the account the agent already runs as needs no switch, got %v %v", id, err)
	}
	if _, err := resolveShellUser("no-such-user-xyz"); err == nil || !strings.Contains(err.Error(), "não encontrado") {
		t.Fatalf("unknown user = %v", err)
	}
	if os.Geteuid() != 0 {
		if _, err := resolveShellUser("root"); err == nil || !strings.Contains(err.Error(), "como root") {
			t.Fatalf("a non-root agent cannot switch user, got %v", err)
		}
	}
}

func TestLookupIdentityReadsTheAccount(t *testing.T) {
	id, err := lookupIdentity("root")
	if err != nil {
		t.Skip("no root account here")
	}
	if id.uid != 0 || id.gid != 0 || id.home == "" || len(id.groups) == 0 {
		t.Fatalf("root = %+v", id)
	}
	if id.shell == "" {
		t.Error("the login shell should come from /etc/passwd")
	}
}

func TestSessionEnvironmentWithholdsTheAgentsSecrets(t *testing.T) {
	agentEnv := []string{
		"RTERMINAL_TOKEN=machine-secret", "RTERMINAL_ANYTHING=1", "INVOCATION_ID=abc", "JOURNAL_STREAM=8:1",
		"LISTEN_FDS=1", "NOTIFY_SOCKET=/run/systemd/notify",
		"HOME=/root", "USER=root", "LOGNAME=root", "SHELL=/bin/bash", "XDG_RUNTIME_DIR=/run/user/0", "SUDO_USER=x",
		"SSH_AUTH_SOCK=/tmp/agent", "PWD=/root", "PATH=/custom/bin", "LANG=pt_BR.UTF-8", "EDITOR=vim",
	}

	// Even without switching user, the agent's credential must not reach a terminal.
	same := envMap(sessionEnvironment(agentEnv, "/bin/zsh", nil))
	for _, k := range []string{"RTERMINAL_TOKEN", "RTERMINAL_ANYTHING", "INVOCATION_ID", "JOURNAL_STREAM", "LISTEN_FDS", "NOTIFY_SOCKET"} {
		if _, ok := same[k]; ok {
			t.Errorf("%s leaked into the terminal", k)
		}
	}
	if same["HOME"] != "/root" || same["PATH"] != "/custom/bin" || same["EDITOR"] != "vim" {
		t.Errorf("an unchanged identity must keep the rest of the environment: %v", same)
	}

	// Switching user replaces the root-specific values with the account's own.
	id := &identity{name: "thiagozs", uid: 1000, gid: 1000, home: "/home/thiagozs"}
	as := envMap(sessionEnvironment(agentEnv, "/usr/bin/zsh", id))
	want := map[string]string{"HOME": "/home/thiagozs", "USER": "thiagozs", "LOGNAME": "thiagozs", "SHELL": "/usr/bin/zsh", "PATH": "/custom/bin", "EDITOR": "vim", "LANG": "pt_BR.UTF-8"}
	for k, v := range want {
		if as[k] != v {
			t.Errorf("%s = %q, want %q", k, as[k], v)
		}
	}
	for _, k := range []string{"XDG_RUNTIME_DIR", "SUDO_USER", "SSH_AUTH_SOCK", "PWD", "RTERMINAL_TOKEN"} {
		if _, ok := as[k]; ok {
			t.Errorf("%s survived the switch to another user", k)
		}
	}
	if as["TERM"] != "xterm-256color" {
		t.Errorf("the terminal basics must still be filled in: TERM=%q", as["TERM"])
	}
}

func TestFilesAreNotAdvertisedWhereTheyCannotRunAsTheUser(t *testing.T) {
	cfg := agentConfig{identity: &identity{name: "x"}}
	has := false
	for _, c := range cfg.capabilities() {
		if c == protocol.CapFiles {
			has = true
		}
	}
	if has != fileIdentitySupported {
		t.Fatalf("files advertised = %v with a switched user, platform support = %v", has, fileIdentitySupported)
	}
}

// --- tests that need root, run in a container (see the README) ---------------

// rootIdentity returns the account named by TEST_SHELL_USER, for tests that
// really switch user. They need a root test process and are skipped otherwise.
func rootIdentity(t *testing.T) *identity {
	t.Helper()
	name := os.Getenv("TEST_SHELL_USER")
	if os.Geteuid() != 0 || name == "" {
		t.Skip("needs root and TEST_SHELL_USER (an unprivileged account)")
	}
	id, err := lookupIdentity(name)
	if err != nil {
		t.Fatal(err)
	}
	if id.uid == 0 {
		t.Fatal("TEST_SHELL_USER must not be root")
	}
	return id
}

func TestShellRunsAsTheChosenUser(t *testing.T) {
	id := rootIdentity(t)
	t.Setenv("RTERMINAL_TOKEN", "machine-secret")
	t.Setenv("INVOCATION_ID", "abc")

	command := exec.Command("/bin/sh")
	command.Env = sessionEnvironment(os.Environ(), "/bin/sh", id)
	command.Dir = startDirectory(command.Env)
	ptmx, err := startShell(command, id)
	if err != nil {
		t.Fatal(err)
	}
	defer ptmx.Close()
	script := "id -u; id -g; id -G; echo HOME=$HOME USER=$USER; env | grep -c -E 'RTERMINAL|INVOCATION'; stat -c 'owner=%U' $(tty); pwd; cat /etc/shadow 2>&1 | head -c 60; echo; exit\n"
	_, _ = ptmx.WriteString(script)
	var out bytes.Buffer
	done := make(chan struct{})
	go func() { _, _ = io.Copy(&out, ptmx); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("the shell did not finish; output so far:\n%s", out.String())
	}
	_ = command.Wait()
	text := strings.ReplaceAll(out.String(), "\r", "")
	for _, want := range []string{
		"\n" + strconv.Itoa(int(id.uid)) + "\n", "\n" + strconv.Itoa(int(id.gid)) + "\n",
		"HOME=" + id.home + " USER=" + id.name, "\n0\n", "owner=" + id.name, "\n" + id.home + "\n", "Permission denied",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("output lacks %q:\n%s", want, text)
		}
	}
	for _, g := range id.groups {
		if g != id.gid && !strings.Contains(text, strconv.Itoa(int(g))) {
			t.Errorf("supplementary group %d is missing from `id -G`:\n%s", g, text)
		}
	}
}

func newFileClientAs(t *testing.T, id *identity) *fileClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := fileUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		_ = runAsIdentity(id, func() error { return serveFiles(ws, id.home) })
	}))
	t.Cleanup(srv.Close)
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close() })
	return &fileClient{t: t, ws: ws}
}

func TestFilesRunAsTheChosenUser(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	id := rootIdentity(t)
	home := id.home
	c := newFileClientAs(t, id)

	// What the account may not touch is refused, although the agent is root.
	c.send(protocol.FileMessage{Op: protocol.OpGet, Path: "/etc/shadow"})
	c.wantError("sem permissão")
	c.send(protocol.FileMessage{Op: protocol.OpList, Path: "/root"})
	c.wantError("sem permissão")
	c.send(protocol.FileMessage{Op: protocol.OpMkdir, Path: "/opt/nope"})
	c.wantError("sem permissão")
	if m := upload(c, "/etc/injected", []byte("x"), false); m.Op != protocol.OpError {
		t.Fatalf("writing in /etc = %+v, want a refusal", m)
	}
	if _, err := os.Stat("/etc/injected"); err == nil {
		t.Fatal("the agent wrote to /etc as root")
	}

	// What it may touch works, and what it creates belongs to it. The first
	// listing opens the account's home although the agent's own home is /root.
	c.send(protocol.FileMessage{Op: protocol.OpList})
	if m := c.reply(); m.Op != protocol.OpList || m.Path != home {
		t.Fatalf("first listing = %+v, want the account's home %s", m, home)
	}
	c.send(protocol.FileMessage{Op: protocol.OpList, Path: home})
	if m := c.reply(); m.Op != protocol.OpList {
		t.Fatalf("home listing = %+v", m)
	}
	target := filepath.Join(home, "uploaded.txt")
	if m := upload(c, target, []byte("hello"), false); m.Op != protocol.OpDone {
		t.Fatalf("upload to home = %+v", m)
	}
	c.send(protocol.FileMessage{Op: protocol.OpMkdir, Path: filepath.Join(home, "newdir")})
	if m := c.reply(); m.Op != protocol.OpDone {
		t.Fatalf("mkdir = %+v", m)
	}
	// A whole tree: every folder created on the way belongs to the account too.
	tree := filepath.Join(home, "tree", "sub", "deeper", "file.txt")
	c.send(protocol.FileMessage{Op: protocol.OpPut, Path: tree, Size: 1, Parents: true})
	if m := c.reply(); m.Op != protocol.OpReady {
		t.Fatalf("put with parents = %+v", m)
	}
	_ = c.ws.WriteMessage(websocket.BinaryMessage, []byte("x"))
	c.send(protocol.FileMessage{Op: protocol.OpEnd})
	if m := c.reply(); m.Op != protocol.OpDone {
		t.Fatalf("tree upload = %+v", m)
	}
	c.send(protocol.FileMessage{Op: protocol.OpPut, Path: "/etc/newtree/file.txt", Size: 1, Parents: true})
	c.wantError("sem permissão")
	if _, err := os.Stat("/etc/newtree"); err == nil {
		t.Fatal("parents created a folder in /etc as root")
	}
	for _, p := range []string{target, filepath.Join(home, "newdir"), filepath.Join(home, "tree"), filepath.Join(home, "tree", "sub", "deeper"), tree} {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if owner := st.Sys().(*syscall.Stat_t).Uid; owner != id.uid {
			t.Errorf("%s is owned by uid %d, want %d", p, owner, id.uid)
		}
	}
}

// The downgrade must stay on the file session's own thread: neither the rest
// of the agent nor later sessions may inherit it.
func TestFileCredentialsDoNotLeakToOtherWork(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	id := rootIdentity(t)
	for i := 0; i < 40; i++ {
		c := newFileClientAs(t, id)
		c.send(protocol.FileMessage{Op: protocol.OpGet, Path: "/etc/shadow"})
		c.wantError("sem permissão")
		c.ws.Close()
		time.Sleep(5 * time.Millisecond)
		// Plain agent work, on whatever thread, must still be root.
		if _, err := os.ReadFile("/etc/shadow"); err != nil {
			t.Fatalf("iteration %d: the agent lost its privileges: %v", i, err)
		}
		plain := newFileClient(t) // a session without a switched user
		plain.send(protocol.FileMessage{Op: protocol.OpGet, Path: "/etc/shadow"})
		if m := plain.reply(); m.Op != protocol.OpGet {
			t.Fatalf("iteration %d: a root file session was downgraded: %+v", i, m)
		}
	}
}

// Deleting, moving and archiving obey the chosen account's permissions too.
func TestFileManagementRunsAsTheChosenUser(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	id := rootIdentity(t)
	c := newFileClientAs(t, id)
	home := id.home

	// What the account may not touch is refused, although the agent is root.
	c.send(protocol.FileMessage{Op: protocol.OpRm, Path: "/etc/hostname"})
	c.wantError("sem permissão")
	if _, err := os.Stat("/etc/hostname"); err != nil {
		t.Fatal("the agent deleted /etc/hostname as root")
	}
	c.send(protocol.FileMessage{Op: protocol.OpMv, Path: "/etc/hostname", To: "/etc/hostname.moved"})
	c.wantError("sem permissão")
	if _, err := os.Stat("/etc/hostname.moved"); err == nil {
		t.Fatal("the agent moved a file inside /etc as root")
	}

	// A zip of a system folder leaves out what the account cannot read.
	c.send(protocol.FileMessage{Op: protocol.OpZip, Paths: []string{"/etc"}})
	if m := c.reply(); m.Op != protocol.OpZip {
		t.Fatalf("zip /etc = %+v", m)
	}
	var data []byte
	var end protocol.FileMessage
	for {
		kind, frame := c.frame()
		if kind == websocket.BinaryMessage {
			data = append(data, frame...)
			continue
		}
		_ = jsonUnmarshal(frame, &end)
		break
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("not a zip: %v", err)
	}
	for _, f := range zr.File {
		if f.Name == "etc/shadow" {
			t.Fatal("/etc/shadow was archived by a root agent for an unprivileged account")
		}
	}
	if end.Op != protocol.OpDone || end.Skipped == 0 {
		t.Fatalf("end = %+v, want some unreadable items skipped", end)
	}

	// Inside its own home everything works, and what it creates is its own.
	work := filepath.Join(home, "work")
	c.send(protocol.FileMessage{Op: protocol.OpMkdir, Path: filepath.Join(work, "sub"), Parents: true})
	if m := c.reply(); m.Op != protocol.OpDone {
		t.Fatalf("mkdir = %+v", m)
	}
	c.send(protocol.FileMessage{Op: protocol.OpMv, Path: filepath.Join(work, "sub"), To: filepath.Join(work, "renamed")})
	if m := c.reply(); m.Op != protocol.OpDone {
		t.Fatalf("mv = %+v", m)
	}
	st, err := os.Stat(filepath.Join(work, "renamed"))
	if err != nil || st.Sys().(*syscall.Stat_t).Uid != id.uid {
		t.Fatalf("renamed folder: %v %v", st, err)
	}
	c.send(protocol.FileMessage{Op: protocol.OpRm, Path: work, Recursive: true})
	if m := c.reply(); m.Op != protocol.OpDone {
		t.Fatalf("rm -r = %+v", m)
	}
	if _, err := os.Stat(work); err == nil {
		t.Fatal("folder still there")
	}
	// The account's home itself is protected.
	c.send(protocol.FileMessage{Op: protocol.OpRm, Path: home, Recursive: true})
	c.wantError("protegida")
}
