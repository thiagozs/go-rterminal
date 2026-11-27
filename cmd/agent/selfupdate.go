package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/thiagozs/go-rterminal/internal/auth"
	"github.com/thiagozs/go-rterminal/internal/diag"
	"github.com/thiagozs/go-rterminal/internal/endpoint"
	"github.com/thiagozs/go-rterminal/internal/protocol"
	"github.com/thiagozs/go-rterminal/internal/tlsutil"
)

const (
	maxBinaryBytes = 100 << 20
	// maxUpdateStarts bounds restarts of a new binary that never confirmed.
	maxUpdateStarts = 3
)

// rollbackAfter is how long a freshly updated agent has to reach the server
// before the previous binary is put back.
var rollbackAfter = 2 * time.Minute

// updateMarker records an update in progress, next to the executable, so the
// new binary knows it must prove itself and the old one can be restored.
type updateMarker struct {
	From      string    `json:"from"`
	To        string    `json:"to"`
	StartedAt time.Time `json:"started_at"`
	Starts    int       `json:"starts"`
}

func markerPath(exe string) string { return exe + ".update" }
func backupPath(exe string) string { return exe + ".old" }

func readMarker(exe string) (updateMarker, bool) {
	var m updateMarker
	data, err := os.ReadFile(markerPath(exe))
	if err != nil || json.Unmarshal(data, &m) != nil {
		return m, false
	}
	return m, true
}

func writeMarker(exe string, m updateMarker) error {
	data, _ := json.Marshal(m)
	return os.WriteFile(markerPath(exe), data, 0o600)
}

// executablePath is the real path of the running binary.
func executablePath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

// updater owns the self-update of one agent process.
type updater struct {
	exe     string
	journal *diag.Journal
	exit    func(code int) // os.Exit; replaced in tests

	mu      sync.Mutex
	busy    bool
	pending *updateMarker // set while a new binary has not confirmed yet
	timer   *time.Timer
}

// begin runs when the agent starts. If it was started by an update it arms
// the safety net: it must reach the server in time, or the old binary returns.
func (u *updater) begin() {
	m, ok := readMarker(u.exe)
	if !ok {
		return
	}
	if m.To != version {
		// Not the new binary: the update never took effect or was rolled back.
		_ = os.Remove(markerPath(u.exe))
		return
	}
	m.Starts++
	if m.Starts > maxUpdateStarts {
		u.rollback(m, fmt.Sprintf("a versão nova reiniciou %d vezes sem conseguir se conectar", m.Starts-1))
		return
	}
	_ = writeMarker(u.exe, m)
	u.mu.Lock()
	u.pending = &m
	u.timer = time.AfterFunc(rollbackAfter, func() {
		u.rollback(m, fmt.Sprintf("a versão nova não conectou ao servidor em %s", rollbackAfter))
	})
	u.mu.Unlock()
}

// confirm runs when the control channel is registered: the update worked.
func (u *updater) confirm() {
	u.mu.Lock()
	m, timer := u.pending, u.timer
	u.pending, u.timer = nil, nil
	u.mu.Unlock()
	if m == nil {
		return
	}
	timer.Stop()
	_ = os.Remove(markerPath(u.exe))
	u.journal.Record("update_ok", fmt.Sprintf("agent atualizado de %s para %s", m.From, m.To))
}

// rollback puts the previous binary back and exits so the supervisor starts it.
func (u *updater) rollback(m updateMarker, why string) {
	_ = os.Remove(markerPath(u.exe))
	if err := os.Rename(backupPath(u.exe), u.exe); err != nil {
		u.journal.Record("update_failed", fmt.Sprintf("%s, e não foi possível restaurar a versão anterior (%v)", why, err))
		return
	}
	u.journal.Record("update_rollback", fmt.Sprintf("%s: voltando para %s", why, m.From))
	log.Printf("[agent] atualização desfeita: %s", why)
	u.exit(0)
}

// download fetches the binary for this machine into a temporary file next to
// the executable and returns its path. The digest must match want.
func downloadAgent(ctx context.Context, config agentConfig, tlsConfig *tls.Config, dir, want string) (string, error) {
	target := runtime.GOOS + "-" + runtime.GOARCH
	endpointURL, err := endpoint.URL(config.serverURL, "/download/agent/"+target, nil, false)
	if err != nil {
		return "", err
	}
	// The agent is configured with a WebSocket URL; the download is plain HTTP(S).
	switch {
	case strings.HasPrefix(endpointURL, "wss://"):
		endpointURL = "https://" + strings.TrimPrefix(endpointURL, "wss://")
	case strings.HasPrefix(endpointURL, "ws://"):
		endpointURL = "http://" + strings.TrimPrefix(endpointURL, "ws://")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpointURL, nil)
	if err != nil {
		return "", err
	}
	auth.SetAuthorization(req.Header, config.authToken)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConfig}}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("baixar: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("o servidor respondeu HTTP %d ao baixar o agent", resp.StatusCode)
	}
	tmp, err := os.CreateTemp(dir, ".agent-update-*")
	if err != nil {
		return "", fmt.Errorf("sem permissão para gravar em %s: %w", dir, errors.Unwrap(err))
	}
	keep := false
	defer func() {
		tmp.Close()
		if !keep {
			os.Remove(tmp.Name())
		}
	}()
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, hash), io.LimitReader(resp.Body, maxBinaryBytes+1))
	if err != nil {
		return "", fmt.Errorf("baixar: %w", err)
	}
	if n > maxBinaryBytes {
		return "", errors.New("o binário baixado é grande demais")
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != want {
		return "", fmt.Errorf("o checksum do binário baixado não confere (esperado %.12s…, recebido %.12s…)", want, got)
	}
	if err := tmp.Chmod(0o755); err != nil {
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	keep = true
	return tmp.Name(), nil
}

// smokeTest runs the new binary to prove it executes on this machine and is
// the version that was asked for.
func smokeTest(ctx context.Context, path, wantVersion string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("o binário novo não executa nesta máquina: %w", err)
	}
	if !strings.Contains(string(out), wantVersion) {
		return fmt.Errorf("o binário novo declara outra versão: %q", strings.TrimSpace(string(out)))
	}
	return nil
}

// install swaps the new binary in, keeping the current one as <exe>.old, and
// records the update so the new process can confirm or undo it.
func (u *updater) install(tmp, to string) error {
	old := backupPath(u.exe)
	_ = os.Remove(old)
	if err := os.Link(u.exe, old); err != nil {
		if err := copyFile(u.exe, old); err != nil {
			return fmt.Errorf("guardar a versão atual: %w", err)
		}
	}
	if err := writeMarker(u.exe, updateMarker{From: version, To: to, StartedAt: time.Now().UTC()}); err != nil {
		return fmt.Errorf("gravar marcador da atualização: %w", err)
	}
	if err := os.Rename(tmp, u.exe); err != nil {
		_ = os.Remove(markerPath(u.exe))
		return fmt.Errorf("trocar o binário: %w", err)
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// unavailable explains why this agent cannot update itself, or returns nil
// when it can. The agent only advertises the capability when this is nil, so
// the panel never offers an update that is bound to fail.
func (u *updater) unavailable(config agentConfig) error {
	switch {
	case config.noSelfUpdate:
		return errors.New("a atualização automática está desativada neste agent (--no-self-update)")
	case runtime.GOOS != "linux":
		return errors.New("a atualização automática só funciona em Linux com systemd")
	case !config.supervised:
		return errors.New("o agent não está rodando como serviço do systemd: ao reiniciar, ninguém o subiria de novo. Use o comando manual")
	}
	return u.writable()
}

// writable checks that the directory of the executable accepts new files,
// which the swap needs (it writes the new binary next to the current one).
func (u *updater) writable() error {
	dir := filepath.Dir(u.exe)
	probe, err := os.CreateTemp(dir, ".agent-update-probe-*")
	if err != nil {
		return fmt.Errorf("o agent roda com o uid %d e não pode gravar em %s, onde está o binário (%v). Use o comando manual (com sudo) ou dê a esse usuário permissão nessa pasta", os.Geteuid(), dir, errors.Unwrap(err))
	}
	probe.Close()
	return os.Remove(probe.Name())
}

// check refuses an update the agent cannot finish safely.
func (u *updater) check(config agentConfig, msg protocol.Message) error {
	if err := u.unavailable(config); err != nil {
		return err
	}
	switch {
	case msg.Version == "" || len(msg.SHA256) != sha256.Size*2:
		return errors.New("pedido de atualização incompleto (faltam versão ou checksum)")
	case msg.Version == version:
		return fmt.Errorf("o agent já está na versão %s", version)
	}
	return nil
}

// run executes an update request. On success it exits, so the supervisor
// starts the new binary; on failure it records why and keeps running.
func (u *updater) run(ctx context.Context, config agentConfig, msg protocol.Message, report func()) {
	u.mu.Lock()
	if u.busy {
		u.mu.Unlock()
		return
	}
	u.busy = true
	u.mu.Unlock()
	fail := func(err error) {
		log.Printf("[agent] atualização recusada ou falhou: %v", err)
		u.journal.Record("update_failed", fmt.Sprintf("atualização para %s falhou: %v", msg.Version, err))
		u.mu.Lock()
		u.busy = false
		u.mu.Unlock()
		report()
	}
	if err := u.check(config, msg); err != nil {
		fail(err)
		return
	}
	tlsConfig, err := tlsutil.ClientConfig(config.tlsCA, config.tlsInsecure)
	if err != nil {
		fail(err)
		return
	}
	log.Printf("[agent] atualizando para %s", msg.Version)
	u.journal.Record("update_started", fmt.Sprintf("atualizando de %s para %s", version, msg.Version))
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	tmp, err := downloadAgent(ctx, config, tlsConfig, filepath.Dir(u.exe), strings.ToLower(msg.SHA256))
	if err != nil {
		fail(err)
		return
	}
	if err := smokeTest(ctx, tmp, msg.Version); err != nil {
		os.Remove(tmp)
		fail(err)
		return
	}
	if err := u.install(tmp, msg.Version); err != nil {
		os.Remove(tmp)
		fail(err)
		return
	}
	u.journal.Record("agent_stop", "reiniciando para concluir a atualização para "+msg.Version)
	report() // deliver the events before the process goes away
	u.exit(0)
}
