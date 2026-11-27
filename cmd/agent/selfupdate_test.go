package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thiagozs/go-rterminal/internal/diag"
	"github.com/thiagozs/go-rterminal/internal/protocol"
)

// fakeBinary is a shell script that behaves like an agent for --version.
func fakeBinary(ver string) []byte {
	return []byte("#!/bin/sh\necho \"agent " + ver + "\"\n")
}

type updateEnv struct {
	t        *testing.T
	exe      string
	exited   atomic.Int32
	exitCode atomic.Int32
	journal  *diag.Journal
	u        *updater
	hits     atomic.Int32
	server   *httptest.Server
	payload  []byte
}

func newUpdateEnv(t *testing.T, payload []byte) *updateEnv {
	t.Helper()
	e := &updateEnv{t: t, payload: payload}
	e.exe = filepath.Join(t.TempDir(), "agent")
	if err := os.WriteFile(e.exe, fakeBinary("current"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.journal = diag.Open("")
	e.u = &updater{exe: e.exe, journal: e.journal, exit: func(code int) { e.exited.Add(1); e.exitCode.Store(int32(code)) }}
	e.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.hits.Add(1)
		if r.Header.Get("Authorization") != "Bearer secret" || r.URL.Path != "/download/agent/"+runtime.GOOS+"-"+runtime.GOARCH {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write(e.payload)
	}))
	t.Cleanup(e.server.Close)
	return e
}

func (e *updateEnv) config() agentConfig {
	return agentConfig{serverURL: "ws" + strings.TrimPrefix(e.server.URL, "http"), authToken: "secret", supervised: true}
}

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func (e *updateEnv) kinds() []string {
	var out []string
	for _, ev := range e.journal.Take(100) {
		out = append(out, ev.Kind)
	}
	return out
}

func (e *updateEnv) read(path string) string {
	data, _ := os.ReadFile(path)
	return string(data)
}

func skipUnlessLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("self-update is Linux only")
	}
}

func TestSelfUpdateInstallsVerifiedBinary(t *testing.T) {
	skipUnlessLinux(t)
	payload := fakeBinary("v2")
	e := newUpdateEnv(t, payload)
	delivered := 0
	e.u.run(context.Background(), e.config(), protocol.Message{Type: protocol.TypeUpdateAgent, Version: "v2", SHA256: sum(payload)}, func() { delivered++ })

	if e.exited.Load() != 1 || e.exitCode.Load() != 0 {
		t.Fatalf("exit calls=%d code=%d; the agent must exit so systemd starts the new binary", e.exited.Load(), e.exitCode.Load())
	}
	if got := e.read(e.exe); got != string(payload) {
		t.Fatalf("binary not replaced: %q", got)
	}
	if got := e.read(backupPath(e.exe)); got != string(fakeBinary("current")) {
		t.Fatalf("previous binary not kept: %q", got)
	}
	if m, ok := readMarker(e.exe); !ok || m.To != "v2" || m.From != version {
		t.Fatalf("marker = %+v ok=%v", m, ok)
	}
	if st, _ := os.Stat(e.exe); st.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v", st.Mode().Perm())
	}
	if delivered == 0 {
		t.Error("events must be delivered before the process exits")
	}
	if k := e.kinds(); len(k) != 2 || k[0] != "update_started" || k[1] != "agent_stop" {
		t.Fatalf("events = %v", k)
	}
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(e.exe), ".agent-update-*"))
	if len(leftovers) != 0 {
		t.Fatalf("temporary files left: %v", leftovers)
	}
}

func TestSelfUpdateRefusesBadDownloads(t *testing.T) {
	skipUnlessLinux(t)
	good := fakeBinary("v2")
	cases := []struct {
		name    string
		payload []byte
		sha     string
		version string
	}{
		{"checksum mismatch", good, sum([]byte("something else")), "v2"},
		{"binary that fails", []byte("#!/bin/sh\nexit 3\n"), "", "v2"},
		{"wrong version inside", fakeBinary("v9"), "", "v2"},
		{"not executable at all", []byte("garbage"), "", "v2"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newUpdateEnv(t, c.payload)
			sha := c.sha
			if sha == "" {
				sha = sum(c.payload)
			}
			e.u.run(context.Background(), e.config(), protocol.Message{Version: c.version, SHA256: sha}, func() {})
			if e.exited.Load() != 0 {
				t.Fatal("must not exit when the update fails")
			}
			if e.read(e.exe) != string(fakeBinary("current")) {
				t.Fatal("the running binary was touched")
			}
			if _, ok := readMarker(e.exe); ok {
				t.Fatal("marker written for a failed update")
			}
			if k := e.kinds(); len(k) != 2 || k[1] != "update_failed" {
				t.Fatalf("events = %v", k)
			}
			if _, err := os.Stat(backupPath(e.exe)); err == nil {
				t.Error("no backup should exist for an update that never started")
			}
		})
	}
}

func TestSelfUpdateRefusals(t *testing.T) {
	skipUnlessLinux(t)
	payload := fakeBinary("v2")
	ok := protocol.Message{Version: "v2", SHA256: sum(payload)}
	cases := []struct {
		name   string
		mutate func(*agentConfig, *protocol.Message)
	}{
		{"opted out", func(c *agentConfig, _ *protocol.Message) { c.noSelfUpdate = true }},
		{"not run by systemd", func(c *agentConfig, _ *protocol.Message) { c.supervised = false }},
		{"no checksum", func(_ *agentConfig, m *protocol.Message) { m.SHA256 = "" }},
		{"short checksum", func(_ *agentConfig, m *protocol.Message) { m.SHA256 = "abc" }},
		{"no version", func(_ *agentConfig, m *protocol.Message) { m.Version = "" }},
		{"already current", func(_ *agentConfig, m *protocol.Message) { m.Version = version }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newUpdateEnv(t, payload)
			cfg, msg := e.config(), ok
			c.mutate(&cfg, &msg)
			e.u.run(context.Background(), cfg, msg, func() {})
			if e.hits.Load() != 0 || e.exited.Load() != 0 || e.read(e.exe) != string(fakeBinary("current")) {
				t.Fatalf("refused update still did something: hits=%d exits=%d", e.hits.Load(), e.exited.Load())
			}
			if k := e.kinds(); len(k) != 1 || k[0] != "update_failed" {
				t.Fatalf("events = %v", k)
			}
		})
	}
}

func TestSelfUpdateRejectedDownload(t *testing.T) {
	skipUnlessLinux(t)
	e := newUpdateEnv(t, fakeBinary("v2"))
	cfg := e.config()
	cfg.authToken = "wrong"
	e.u.run(context.Background(), cfg, protocol.Message{Version: "v2", SHA256: sum(fakeBinary("v2"))}, func() {})
	if e.exited.Load() != 0 || e.read(e.exe) != string(fakeBinary("current")) {
		t.Fatal("a 401 must leave everything alone")
	}
}

func withVersion(t *testing.T, v string) {
	t.Helper()
	old := version
	version = v
	t.Cleanup(func() { version = old })
}

func TestNewBinaryConfirmsAfterConnecting(t *testing.T) {
	withVersion(t, "v2")
	e := newUpdateEnv(t, nil)
	_ = writeMarker(e.exe, updateMarker{From: "v1", To: "v2"})
	e.u.begin()
	if m, _ := readMarker(e.exe); m.Starts != 1 {
		t.Fatalf("starts = %d, want 1", m.Starts)
	}
	e.u.confirm()
	if _, ok := readMarker(e.exe); ok {
		t.Fatal("marker must go away once the agent connected")
	}
	if k := e.kinds(); len(k) != 1 || k[0] != "update_ok" {
		t.Fatalf("events = %v", k)
	}
	if e.exited.Load() != 0 {
		t.Fatal("a confirmed update must not exit")
	}
	e.u.confirm() // a second call does nothing
}

func TestStaleMarkerIsDropped(t *testing.T) {
	withVersion(t, "v1")
	e := newUpdateEnv(t, nil)
	_ = writeMarker(e.exe, updateMarker{From: "v1", To: "v2"})
	e.u.begin() // this is the old binary: the update never took effect
	if _, ok := readMarker(e.exe); ok {
		t.Fatal("stale marker kept")
	}
	if e.exited.Load() != 0 || len(e.kinds()) != 0 {
		t.Fatal("a stale marker must be ignored quietly")
	}
}

func TestRollbackWhenNewBinaryNeverConnects(t *testing.T) {
	withVersion(t, "v2")
	old := rollbackAfter
	rollbackAfter = 50 * time.Millisecond
	t.Cleanup(func() { rollbackAfter = old })
	e := newUpdateEnv(t, nil)
	if err := os.WriteFile(backupPath(e.exe), fakeBinary("previous"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = writeMarker(e.exe, updateMarker{From: "v1", To: "v2"})
	e.u.begin()
	deadline := time.Now().Add(3 * time.Second)
	for e.exited.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if e.exited.Load() != 1 {
		t.Fatal("no rollback after the deadline")
	}
	if e.read(e.exe) != string(fakeBinary("previous")) {
		t.Fatalf("previous binary not restored: %q", e.read(e.exe))
	}
	if _, ok := readMarker(e.exe); ok {
		t.Fatal("marker kept after rollback")
	}
	if k := e.kinds(); len(k) != 1 || k[0] != "update_rollback" {
		t.Fatalf("events = %v", k)
	}
}

func TestRollbackAfterCrashLoop(t *testing.T) {
	withVersion(t, "v2")
	e := newUpdateEnv(t, nil)
	_ = os.WriteFile(backupPath(e.exe), fakeBinary("previous"), 0o755)
	_ = writeMarker(e.exe, updateMarker{From: "v1", To: "v2", Starts: maxUpdateStarts})
	e.u.begin()
	if e.exited.Load() != 1 || e.read(e.exe) != string(fakeBinary("previous")) {
		t.Fatalf("crash loop not rolled back: exits=%d", e.exited.Load())
	}
}

func TestConfirmedUpdateIsNotRolledBack(t *testing.T) {
	withVersion(t, "v2")
	old := rollbackAfter
	rollbackAfter = 50 * time.Millisecond
	t.Cleanup(func() { rollbackAfter = old })
	e := newUpdateEnv(t, nil)
	_ = os.WriteFile(backupPath(e.exe), fakeBinary("previous"), 0o755)
	_ = writeMarker(e.exe, updateMarker{From: "v1", To: "v2"})
	e.u.begin()
	e.u.confirm()
	time.Sleep(200 * time.Millisecond)
	if e.exited.Load() != 0 || e.read(e.exe) != string(fakeBinary("current")) {
		t.Fatal("rolled back an update that had connected")
	}
}

func TestSelfUpdateIsOnlyAdvertisedWhenPossible(t *testing.T) {
	skipUnlessLinux(t)
	e := newUpdateEnv(t, nil)
	has := func(cfg agentConfig, cap string) bool {
		for _, c := range cfg.capabilities() {
			if c == cap {
				return true
			}
		}
		return false
	}
	cfg := e.config()
	cfg.updater = e.u
	if !has(cfg, protocol.CapSelfUpdate) {
		t.Fatal("a supervised agent that can write next to its binary must advertise self-update")
	}
	if !has(cfg, protocol.CapFiles) {
		t.Error("files must be advertised by default")
	}

	notSupervised := cfg
	notSupervised.supervised = false
	optedOut := cfg
	optedOut.noSelfUpdate = true
	noFiles := cfg
	noFiles.noFiles = true
	noUpdater := e.config()
	for name, c := range map[string]agentConfig{"not under systemd": notSupervised, "opted out": optedOut, "no updater": noUpdater} {
		if has(c, protocol.CapSelfUpdate) {
			t.Errorf("%s: advertises self-update although it cannot", name)
		}
	}
	if has(noFiles, protocol.CapFiles) || !has(noFiles, protocol.CapSelfUpdate) {
		t.Error("--no-files must only remove the files capability")
	}
}

func TestSelfUpdateNotAdvertisedWhenBinaryFolderIsReadOnly(t *testing.T) {
	skipUnlessLinux(t)
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	e := newUpdateEnv(t, fakeBinary("v2"))
	dir := filepath.Dir(e.exe)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	cfg := e.config()
	cfg.updater = e.u

	err := e.u.unavailable(cfg)
	if err == nil || !strings.Contains(err.Error(), "não pode gravar em "+dir) || !strings.Contains(err.Error(), "comando manual") {
		t.Fatalf("reason = %v, want an explanation with the folder and the way out", err)
	}
	for _, c := range cfg.capabilities() {
		if c == protocol.CapSelfUpdate {
			t.Fatal("must not advertise self-update from a read-only folder")
		}
	}
	// A request that arrives anyway is refused before any download.
	e.u.run(context.Background(), cfg, protocol.Message{Version: "v2", SHA256: sum(fakeBinary("v2"))}, func() {})
	if e.hits.Load() != 0 || e.exited.Load() != 0 {
		t.Fatal("downloaded or exited although the folder is read-only")
	}
	if k := e.kinds(); len(k) != 1 || k[0] != "update_failed" {
		t.Fatalf("events = %v", k)
	}
}
