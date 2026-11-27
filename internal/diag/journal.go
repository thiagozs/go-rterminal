// Package diag lets an agent keep track of why it was offline: it writes the
// connection events to a local file, so the history survives restarts, and
// queues them for the server, so they can be read in the panel afterwards.
package diag

import (
	"bufio"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/thiagozs/go-rterminal/internal/protocol"
)

const (
	maxFileBytes   = 1 << 20 // the file rotates to <file>.1 past this
	maxPending     = 500     // events waiting for the server
	maxDetailBytes = 600
	// BatchSize is how many events fit in one message to the server.
	BatchSize = 200
)

// Journal records events. A nil *Journal is valid and records nothing.
type Journal struct {
	mu      sync.Mutex
	path    string
	size    int64
	pending []protocol.Event
	last    string // kind of the last event of the previous run
}

// Open prepares the journal at path. An empty path keeps events in memory
// only. A file that cannot be written is not fatal: the problem is logged and
// the journal still queues events for the server.
func Open(path string) *Journal {
	j := &Journal{path: path}
	if path == "" {
		return j
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		log.Printf("[agent] diário de eventos desativado (%v)", err)
		j.path = ""
		return j
	}
	j.last = lastKind(path)
	if st, err := os.Stat(path); err == nil {
		j.size = st.Size()
	}
	return j
}

// PreviousRunEndedCleanly reports whether the last run of the agent said
// goodbye. ok is false when there is no earlier run on record.
func (j *Journal) PreviousRunEndedCleanly() (clean, ok bool) {
	if j == nil || j.last == "" {
		return false, false
	}
	return j.last == "agent_stop", true
}

func trim(s string) string {
	if len(s) > maxDetailBytes {
		return s[:maxDetailBytes] + "…"
	}
	return s
}

// Record adds an event to the file and to the queue for the server.
func (j *Journal) Record(kind, detail string) {
	if j == nil {
		return
	}
	ev := protocol.Event{At: time.Now().UTC(), Kind: kind, Detail: trim(detail)}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.pending = append(j.pending, ev)
	if over := len(j.pending) - maxPending; over > 0 {
		j.pending = append([]protocol.Event(nil), j.pending[over:]...)
	}
	j.write(ev)
}

func (j *Journal) write(ev protocol.Event) {
	if j.path == "" {
		return
	}
	line, _ := json.Marshal(ev)
	line = append(line, '\n')
	if j.size+int64(len(line)) > maxFileBytes {
		_ = os.Rename(j.path, j.path+".1")
		j.size = 0
	}
	f, err := os.OpenFile(j.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	if n, err := f.Write(line); err == nil {
		j.size += int64(n)
	}
}

// Take removes and returns up to n queued events, oldest first.
func (j *Journal) Take(n int) []protocol.Event {
	if j == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if n > len(j.pending) {
		n = len(j.pending)
	}
	out := append([]protocol.Event(nil), j.pending[:n]...)
	j.pending = append([]protocol.Event(nil), j.pending[n:]...)
	return out
}

// Requeue puts events back at the front, after a failed send.
func (j *Journal) Requeue(events []protocol.Event) {
	if j == nil || len(events) == 0 {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.pending = append(append([]protocol.Event(nil), events...), j.pending...)
	if over := len(j.pending) - maxPending; over > 0 {
		j.pending = append([]protocol.Event(nil), j.pending[over:]...)
	}
}

// lastKind returns the kind of the last event in the file ("" if none).
func lastKind(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() == 0 {
		return ""
	}
	const tail = 8 << 10
	if st.Size() > tail {
		_, _ = f.Seek(st.Size()-tail, 0)
	}
	var kind string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		var ev protocol.Event
		if json.Unmarshal(sc.Bytes(), &ev) == nil && ev.Kind != "" {
			kind = ev.Kind
		}
	}
	return kind
}
