package recording

import (
	"bufio"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thiagozs/go-rterminal/internal/store"
	"golang.org/x/crypto/bcrypt"
)

func newManager(t *testing.T, maxBytes int64) (*Manager, *store.Store) {
	t.Helper()
	store.BcryptCost = bcrypt.MinCost
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m, err := NewManager(filepath.Join(t.TempDir(), "rec"), st, maxBytes, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	return m, st
}

type cast struct {
	header map[string]any
	events [][3]any
}

func readCast(t *testing.T, m *Manager, id string) cast {
	t.Helper()
	f, err := m.Open(id)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	var c cast
	if !sc.Scan() || json.Unmarshal(sc.Bytes(), &c.header) != nil {
		t.Fatal("bad header")
	}
	for sc.Scan() {
		var ev [3]any
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatalf("bad event %q: %v", sc.Text(), err)
		}
		c.events = append(c.events, ev)
	}
	return c
}

func (c cast) text() string {
	var b strings.Builder
	for _, ev := range c.events {
		if ev[1] == "o" {
			b.WriteString(ev[2].(string))
		}
	}
	return b.String()
}

func TestRecordsAsciicastWithSizeAndCatalogue(t *testing.T) {
	m, st := newManager(t, 0)
	r := m.Start(Meta{MachineID: "pi", MachineName: "Raspberry", Username: "ana"})
	r.Resize(120, 40) // first size goes to the header
	r.Output([]byte("hello \x1b[32mworld\x1b[0m\r\n"))
	r.Resize(90, 20)
	r.Output([]byte("<b>&</b>\r\n"))
	r.Close()
	r.Close() // idempotent

	c := readCast(t, m, r.ID())
	if c.header["version"] != float64(2) || c.header["width"] != float64(120) || c.header["height"] != float64(40) {
		t.Fatalf("header = %v", c.header)
	}
	if got := c.text(); got != "hello \x1b[32mworld\x1b[0m\r\n<b>&</b>\r\n" {
		t.Fatalf("text = %q", got)
	}
	var resizes []string
	for _, ev := range c.events {
		if ev[1] == "r" {
			resizes = append(resizes, ev[2].(string))
		}
	}
	if len(resizes) != 1 || resizes[0] != "90x20" {
		t.Fatalf("resize events = %v", resizes)
	}
	rec, err := st.GetRecording(r.ID())
	if err != nil || rec.Username != "ana" || rec.MachineName != "Raspberry" || rec.EndedAt == nil || rec.Bytes <= 0 || rec.Truncated {
		t.Fatalf("catalogue = %+v, %v", rec, err)
	}
	info, _ := os.Stat(filepath.Join(m.Dir(), r.ID()+".cast"))
	if info.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestMultibyteCharactersSplitAcrossChunksSurvive(t *testing.T) {
	m, _ := newManager(t, 0)
	r := m.Start(Meta{MachineID: "x", MachineName: "x", Username: "u"})
	icon := []byte("→ \xef\x84\xa0 ação") // arrow, Nerd Font glyph, accents
	for i := 0; i < len(icon); i++ {      // worst case: one byte per chunk
		r.Output(icon[i : i+1])
	}
	r.Close()
	if got := readCast(t, m, r.ID()).text(); got != string(icon) {
		t.Fatalf("text = %q, want %q", got, icon)
	}
}

func TestNothingIsLeftWhenTheSessionNeverStarts(t *testing.T) {
	m, st := newManager(t, 0)
	m.Start(Meta{MachineID: "x", MachineName: "x", Username: "u"}).Close()
	if list, _ := st.ListRecordings("", 10); len(list) != 0 {
		t.Fatalf("catalogue = %v", list)
	}
	if entries, _ := os.ReadDir(m.Dir()); len(entries) != 0 {
		t.Fatalf("files = %v", entries)
	}
}

func TestSizeLimitCutsTheRecordingWithANotice(t *testing.T) {
	m, st := newManager(t, 600)
	r := m.Start(Meta{MachineID: "x", MachineName: "x", Username: "u"})
	for i := 0; i < 50; i++ {
		r.Output([]byte(strings.Repeat("a", 40) + "\r\n"))
	}
	r.Close()
	c := readCast(t, m, r.ID())
	if !strings.Contains(c.text(), "limite de tamanho") {
		t.Fatalf("no notice in %q", c.text())
	}
	rec, _ := st.GetRecording(r.ID())
	if !rec.Truncated || rec.Bytes > 600+200 {
		t.Fatalf("catalogue = %+v", rec)
	}
}

func TestIdsOutsideTheDirectoryAreRefused(t *testing.T) {
	m, _ := newManager(t, 0)
	for _, id := range []string{"../../etc/passwd", "..", "", "abc", "00000000-0000-0000-0000-000000000000/../x"} {
		if _, err := m.Open(id); err != store.ErrNotFound {
			t.Errorf("Open(%q) = %v, want ErrNotFound", id, err)
		}
		if err := m.Delete(id); err != store.ErrNotFound {
			t.Errorf("Delete(%q) = %v, want ErrNotFound", id, err)
		}
	}
}

func TestPurgeAndRecover(t *testing.T) {
	m, st := newManager(t, 0)
	old := m.Start(Meta{MachineID: "x", MachineName: "x", Username: "u"})
	old.Output([]byte("old"))
	old.Close()
	running := m.Start(Meta{MachineID: "x", MachineName: "x", Username: "u"})
	running.Output([]byte("still going when the server died"))
	// no Close: simulates a crash

	m.Recover()
	rec, _ := st.GetRecording(running.ID())
	if rec.EndedAt == nil || rec.Bytes == 0 {
		t.Fatalf("not recovered: %+v", rec)
	}

	time.Sleep(1100 * time.Millisecond) // started_at has second resolution
	n, err := m.Purge(time.Now())
	if err != nil || n != 2 {
		t.Fatalf("purged %d, %v; want 2", n, err)
	}
	if entries, _ := os.ReadDir(m.Dir()); len(entries) != 0 {
		t.Fatalf("files left: %v", entries)
	}
	if _, err := st.GetRecording(old.ID()); err != store.ErrNotFound {
		t.Fatalf("catalogue entry left: %v", err)
	}
}
