package diag

import (
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestJournalWritesAndQueues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "events.jsonl")
	j := Open(path)
	j.Record("connected", "ok")
	j.Record("lost", "caiu")
	data, err := os.ReadFile(path)
	if err != nil || strings.Count(string(data), "\n") != 2 || !strings.Contains(string(data), `"kind":"lost"`) {
		t.Fatalf("file = %q err=%v", data, err)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", st.Mode().Perm())
	}
	got := j.Take(1)
	if len(got) != 1 || got[0].Kind != "connected" {
		t.Fatalf("take = %+v", got)
	}
	j.Requeue(got)
	all := j.Take(10)
	if len(all) != 2 || all[0].Kind != "connected" || all[1].Kind != "lost" {
		t.Fatalf("after requeue = %+v", all)
	}
	if len(j.Take(10)) != 0 {
		t.Fatal("queue not empty")
	}
}

func TestJournalRotatesAndBoundsQueue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "e.jsonl")
	j := Open(path)
	big := strings.Repeat("x", 500)
	for i := 0; i < 3000; i++ {
		j.Record("k", big)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("no rotated file: %v", err)
	}
	if st, _ := os.Stat(path); st.Size() > maxFileBytes {
		t.Fatalf("file grew to %d", st.Size())
	}
	if n := len(j.Take(10000)); n != maxPending {
		t.Fatalf("pending = %d, want %d", n, maxPending)
	}
	j.Record("k", strings.Repeat("y", 5000))
	if d := j.Take(1)[0].Detail; len(d) > maxDetailBytes+4 {
		t.Fatalf("detail not trimmed: %d", len(d))
	}
}

func TestPreviousRunEndedCleanly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "e.jsonl")
	if _, ok := Open(path).PreviousRunEndedCleanly(); ok {
		t.Fatal("first run has no history")
	}
	j := Open(path)
	j.Record("connected", "")
	if clean, ok := Open(path).PreviousRunEndedCleanly(); !ok || clean {
		t.Fatalf("unclean run: clean=%v ok=%v", clean, ok)
	}
	j.Record("agent_stop", "")
	if clean, ok := Open(path).PreviousRunEndedCleanly(); !ok || !clean {
		t.Fatalf("clean run: clean=%v ok=%v", clean, ok)
	}
}

func TestNilAndUnwritableJournalsAreHarmless(t *testing.T) {
	var j *Journal
	j.Record("k", "d")
	j.Requeue(nil)
	if j.Take(1) != nil {
		t.Fatal("nil journal returned events")
	}
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	_ = os.WriteFile(blocker, nil, 0o600)
	bad := Open(filepath.Join(blocker, "x", "e.jsonl")) // parent is a file
	bad.Record("k", "d")
	if len(bad.Take(5)) != 1 {
		t.Fatal("events must still be queued when the file is unusable")
	}
}

func TestSuspendedFor(t *testing.T) {
	if _, ok := suspendedFor(5*time.Second, 5*time.Second); ok {
		t.Error("normal tick flagged")
	}
	if _, ok := suspendedFor(7*time.Second, 5*time.Second); ok {
		t.Error("small drift flagged")
	}
	gap, ok := suspendedFor(20*time.Minute+5*time.Second, 5*time.Second)
	if !ok || gap != 20*time.Minute {
		t.Errorf("gap=%v ok=%v", gap, ok)
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestDescribe(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{NewDialError(websocket.ErrBadHandshake, &http.Response{StatusCode: 502}), "HTTP 502"},
		{NewDialError(websocket.ErrBadHandshake, &http.Response{StatusCode: 401}), "credencial recusada"},
		{&net.OpError{Op: "dial", Err: &net.DNSError{Err: "no such host", IsNotFound: true}}, "DNS"},
		{&net.OpError{Op: "dial", Err: syscall.ENETUNREACH}, "rede inacessível"},
		{fmt.Errorf("dial: %w", syscall.ECONNREFUSED), "conexão recusada"},
		{x509.UnknownAuthorityError{}, "certificado TLS"},
		{&websocket.CloseError{Code: websocket.CloseAbnormalClosure}, "interrompida sem aviso"},
		{&websocket.CloseError{Code: websocket.ClosePolicyViolation, Text: "agent already connected"}, "agent already connected"},
		{timeoutErr{}, "tempo esgotado"},
		{io.ErrUnexpectedEOF, "encerrada pelo outro lado"},
		{errors.New("algo novo"), "algo novo"},
	}
	for _, c := range cases {
		if got := Describe(c.err); !strings.Contains(got, c.want) {
			t.Errorf("Describe(%v) = %q, want it to contain %q", c.err, got, c.want)
		}
	}
	if Describe(nil) != "" {
		t.Error("nil must describe as empty")
	}
}

func TestNetSnapshot(t *testing.T) {
	dir := t.TempDir()
	sys := filepath.Join(dir, "net")
	for name, state := range map[string]string{"wlp2s0": "up\n", "enp3s0": "down\n", "lo": "unknown\n", "docker0": "up\n", "veth12": "up\n"} {
		_ = os.MkdirAll(filepath.Join(sys, name), 0o755)
		_ = os.WriteFile(filepath.Join(sys, name, "operstate"), []byte(state), 0o644)
	}
	wireless := filepath.Join(dir, "wireless")
	_ = os.WriteFile(wireless, []byte("Inter-| sta-|   Quality        |\n face | tus | link level noise |\nwlp2s0: 0000   62.  -48.  -256        0      0\n"), 0o644)
	route := filepath.Join(dir, "route")
	_ = os.WriteFile(route, []byte("Iface\tDestination\tGateway\nwlp2s0\t00000000\t0101A8C0\nwlp2s0\t0001A8C0\t00000000\n"), 0o644)

	got := netSnapshot(sys, wireless, route)
	for _, want := range []string{"wlp2s0 up (sinal -48 dBm)", "enp3s0 down", "rota padrão por wlp2s0"} {
		if !strings.Contains(got, want) {
			t.Errorf("snapshot %q lacks %q", got, want)
		}
	}
	for _, unwanted := range []string{"docker0", "veth12", "lo "} {
		if strings.Contains(got, unwanted) {
			t.Errorf("snapshot %q should skip %q", got, unwanted)
		}
	}
	if got := netSnapshot(filepath.Join(dir, "none"), "", ""); !strings.Contains(got, "nenhuma") || !strings.Contains(got, "sem rota padrão") {
		t.Errorf("empty snapshot = %q", got)
	}
}
