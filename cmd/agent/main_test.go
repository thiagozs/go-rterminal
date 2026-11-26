package main

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/thiagozs/go-rterminal/internal/protocol"
	"github.com/thiagozs/go-rterminal/internal/relay"
)

func TestSemaphore(t *testing.T) {
	slots := make(semaphore, 2)
	if !slots.tryAcquire() || !slots.tryAcquire() {
		t.Fatal("expected two slots to be available")
	}
	if slots.tryAcquire() {
		t.Fatal("third slot should be refused")
	}
	slots.release()
	if !slots.tryAcquire() {
		t.Fatal("released slot should be reusable")
	}
}

func TestNilSemaphoreIsUnlimited(t *testing.T) {
	var slots semaphore
	for i := 0; i < 100; i++ {
		if !slots.tryAcquire() {
			t.Fatal("unlimited semaphore refused a slot")
		}
	}
	slots.release()
}

func newSessionPair(t *testing.T, script string) (agentDone <-chan error, peer *websocket.Conn) {
	done, dial := newSession(t, script, agentConfig{})
	return done, dial()
}

// newSession starts shellSession on the agent side and returns a function that
// joins the other side of the session, which triggers pairing.
func newSession(t *testing.T, script string, config agentConfig) (agentDone <-chan error, dial func() *websocket.Conn) {
	t.Helper()
	srv := httptest.NewServer(relay.New(relay.Config{
		AuthToken: "secret", SessionTimeout: 5 * time.Second, Logger: log.New(io.Discard, "", 0),
	}).Handler())
	t.Cleanup(srv.Close)

	// Create a session through the control endpoint's session registry by
	// registering a fake agent and requesting a session.
	wsBase := "ws" + strings.TrimPrefix(srv.URL, "http")
	header := http.Header{"Authorization": {"Bearer secret"}}
	ctrl, _, err := websocket.DefaultDialer.Dial(wsBase+"/agent", header)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ctrl.Close() })
	if err := ctrl.WriteJSON(protocol.Message{Type: protocol.TypeRegisterAgent, AgentID: "a"}); err != nil {
		t.Fatal(err)
	}
	var sessionID string
	for i := 0; i < 50 && sessionID == ""; i++ {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/client?agent=a", nil)
		req.Header.Set("Authorization", "Bearer secret")
		if resp, err := http.DefaultClient.Do(req); err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				sessionID = strings.TrimSpace(string(body))
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if sessionID == "" {
		t.Fatal("could not create session")
	}

	path := filepath.Join(t.TempDir(), "shell.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	config.serverURL, config.shellPath, config.authToken = srv.URL, path, "secret"
	done := make(chan error, 1)
	go func() { done <- shellSession(context.Background(), config, sessionID, nil) }()
	return done, func() *websocket.Conn {
		peer, _, err := websocket.DefaultDialer.Dial(wsBase+"/session?id="+sessionID, header)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { peer.Close() })
		return peer
	}
}

func TestShellExitFlushesAllOutput(t *testing.T) {
	done, peer := newSessionPair(t, "i=0; while [ $i -lt 2000 ]; do echo line-$i; i=$((i+1)); done; exit 0\n")
	var out strings.Builder
	_ = peer.SetReadDeadline(time.Now().Add(10 * time.Second))
	var closeErr error
	for {
		_, data, err := peer.ReadMessage()
		if err != nil {
			closeErr = err
			break
		}
		out.Write(data)
	}
	if !websocket.IsCloseError(closeErr, websocket.CloseNormalClosure) {
		t.Fatalf("close error = %v, want normal closure", closeErr)
	}
	if !strings.Contains(out.String(), "line-1999") {
		t.Fatalf("last output lost; got %d bytes", out.Len())
	}
	if err := <-done; err != nil {
		t.Fatalf("shellSession: %v", err)
	}
}

func TestClosingSessionKillsChildProcesses(t *testing.T) {
	// Plain background job (same process group as the shell).
	assertChildKilledOnClose(t, "sleep 300 &\n")
}

func TestClosingSessionKillsBackgroundJobsInOwnProcessGroup(t *testing.T) {
	// With job control (as in an interactive shell) background jobs get their
	// own process group, which killing the shell's group alone would miss.
	assertChildKilledOnClose(t, "set -m\nsleep 300 &\n")
}

func assertChildKilledOnClose(t *testing.T, startChild string) {
	t.Helper()
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	done, peer := newSessionPair(t, startChild+"echo $! > "+pidFile+"\nwait\n")
	var pid int
	for i := 0; i < 100 && pid == 0; i++ {
		if data, err := os.ReadFile(pidFile); err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(data)))
		}
		time.Sleep(50 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("child pid not written")
	}
	peer.Close()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("shellSession did not return after peer closed")
	}
	for i := 0; i < 50; i++ {
		if err := syscall.Kill(pid, 0); err != nil { // ESRCH: gone
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("child process %d still running", pid)
}

func TestShellStartsOnlyAfterPairing(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "started")
	done, dial := newSession(t, "touch "+marker+"\nsleep 5\n", agentConfig{})
	time.Sleep(600 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("shell started before the session was paired")
	}
	peer := dial()
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(marker); err == nil {
			peer.Close()
			<-done
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("shell did not start after pairing")
}

func TestUnpairedSessionDoesNotUseShellSlot(t *testing.T) {
	slots := make(semaphore, 1)
	pending := make(semaphore, 4)
	pending.tryAcquire()
	config := agentConfig{sessionSlots: slots, pendingSlots: pending}
	_, _ = newSession(t, "sleep 5\n", config)
	time.Sleep(400 * time.Millisecond)
	if !slots.tryAcquire() {
		t.Fatal("unpaired session is holding a shell slot")
	}
}

func TestBusyAgentClosesWithTryAgainLater(t *testing.T) {
	slots := make(semaphore, 1)
	slots.tryAcquire() // the only slot is taken
	pending := make(semaphore, 4)
	pending.tryAcquire()
	done, dial := newSession(t, "sleep 5\n", agentConfig{sessionSlots: slots, pendingSlots: pending})
	peer := dial()
	_ = peer.SetReadDeadline(time.Now().Add(5 * time.Second))
	var err error
	for err == nil {
		_, _, err = peer.ReadMessage()
		if err == nil {
			continue
		}
	}
	if !websocket.IsCloseError(err, websocket.CloseTryAgainLater) {
		t.Fatalf("close error = %v, want try-again-later", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("shellSession: %v", err)
	}
}
