// Package recording stores web terminal sessions in the asciicast v2 format
// (https://docs.asciinema.org/manual/asciicast/v2/), so they can be replayed
// in the panel or with the asciinema player.
//
// Only what the shell prints is recorded, never what the user types: text
// typed at a no-echo prompt (sudo, ssh, a password manager) therefore never
// reaches the file. Commands still appear because the shell echoes them.
package recording

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/thiagozs/go-rterminal/internal/store"
)

// DefaultMaxBytes caps one recording; a session that prints more is cut with a notice.
const DefaultMaxBytes = 50 << 20

// Meta says who recorded what.
type Meta struct {
	MachineID   string
	MachineName string
	Username    string
}

type Manager struct {
	dir      string
	store    *store.Store
	maxBytes int64
	logger   *log.Logger
}

// NewManager prepares dir (created private) and returns a manager that keeps
// the catalogue in st. maxBytes <= 0 uses DefaultMaxBytes.
func NewManager(dir string, st *store.Store, maxBytes int64, logger *log.Logger) (*Manager, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	if logger == nil {
		logger = log.Default()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("criar pasta de gravações: %w", err)
	}
	return &Manager{dir: dir, store: st, maxBytes: maxBytes, logger: logger}, nil
}

func (m *Manager) Dir() string { return m.dir }

// path returns the file of a recording, refusing anything that is not a UUID
// so an id taken from a request can never point outside the directory.
func (m *Manager) path(id string) (string, bool) {
	u, err := uuid.Parse(id)
	if err != nil || u.String() != id {
		return "", false
	}
	return filepath.Join(m.dir, id+".cast"), true
}

// Open returns the recording file for reading.
func (m *Manager) Open(id string) (*os.File, error) {
	p, ok := m.path(id)
	if !ok {
		return nil, store.ErrNotFound
	}
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, store.ErrNotFound
	}
	return f, err
}

// Delete removes the file and the catalogue entry.
func (m *Manager) Delete(id string) error {
	p, ok := m.path(id)
	if !ok {
		return store.ErrNotFound
	}
	if err := m.store.DeleteRecording(id); err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Purge deletes recordings that started before the given time.
func (m *Manager) Purge(before time.Time) (int, error) {
	ids, err := m.store.RecordingsStartedBefore(before)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		if err := m.Delete(id); err != nil {
			m.logger.Printf("[recording] falha ao apagar %s: %v", id, err)
			continue
		}
		n++
	}
	return n, nil
}

// Recover closes the entries a crash or restart left running, using the
// file's size and last write time. Call it before serving.
func (m *Manager) Recover() {
	list, err := m.store.UnfinishedRecordings()
	if err != nil {
		m.logger.Printf("[recording] falha ao recuperar gravações: %v", err)
		return
	}
	for _, rec := range list {
		ended, size := rec.StartedAt, int64(0)
		if p, ok := m.path(rec.ID); ok {
			if info, err := os.Stat(p); err == nil {
				ended, size = info.ModTime(), info.Size()
			}
		}
		if err := m.store.FinishRecording(rec.ID, ended, size, false); err != nil {
			m.logger.Printf("[recording] falha ao fechar %s: %v", rec.ID, err)
		}
	}
}

// Recorder records one session. It implements relay.Tap. The file and the
// catalogue entry are created by the first event, so a session that never
// started leaves nothing behind.
type Recorder struct {
	m    *Manager
	meta Meta
	id   string

	mu        sync.Mutex
	file      *os.File
	started   time.Time
	size      int64
	truncated bool
	closed    bool
	pending   []byte // incomplete UTF-8 sequence at the end of the last chunk
}

// Start returns a recorder for a new session.
func (m *Manager) Start(meta Meta) *Recorder {
	return &Recorder{m: m, meta: meta, id: uuid.NewString()}
}

// ID identifies the recording in the panel.
func (r *Recorder) ID() string { return r.id }

// open creates the file with the asciicast header; the caller holds r.mu.
func (r *Recorder) open(cols, rows int) bool {
	if r.file != nil {
		return true
	}
	if r.closed {
		return false
	}
	p, _ := r.m.path(r.id)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		r.m.logger.Printf("[recording] não consegui criar %s: %v", p, err)
		r.closed = true
		return false
	}
	r.started = time.Now()
	header, _ := json.Marshal(map[string]any{
		"version": 2, "width": cols, "height": rows, "timestamp": r.started.Unix(),
		"title": r.meta.MachineName + " (" + r.meta.Username + ")",
		"env":   map[string]string{"TERM": "xterm-256color"},
	})
	n, err := f.Write(append(header, '\n'))
	if err != nil {
		r.m.logger.Printf("[recording] falha ao escrever %s: %v", p, err)
		f.Close()
		os.Remove(p)
		r.closed = true
		return false
	}
	r.size = int64(n)
	if err := r.m.store.AddRecording(store.Recording{ID: r.id, MachineID: r.meta.MachineID,
		MachineName: r.meta.MachineName, Username: r.meta.Username, StartedAt: r.started}); err != nil {
		r.m.logger.Printf("[recording] falha ao registrar %s: %v", r.id, err)
		f.Close()
		os.Remove(p)
		r.closed = true
		return false
	}
	r.file = f
	return true
}

// event appends one asciicast event; the caller holds r.mu.
func (r *Recorder) event(kind, data string) {
	if r.file == nil || r.truncated {
		return
	}
	elapsed := math.Round(time.Since(r.started).Seconds()*1000) / 1000
	var line bytes.Buffer
	enc := json.NewEncoder(&line) // Encode ends the line with \n
	enc.SetEscapeHTML(false)
	_ = enc.Encode([]any{elapsed, kind, data})
	if r.size+int64(line.Len()) > r.m.maxBytes {
		r.truncated = true
		notice, _ := json.Marshal([]any{elapsed, "o", "\r\n[gravação interrompida: limite de tamanho atingido]\r\n"})
		_, _ = r.file.Write(append(notice, '\n'))
		return
	}
	n, err := r.file.Write(line.Bytes())
	r.size += int64(n)
	if err != nil {
		r.truncated = true
		r.m.logger.Printf("[recording] falha ao escrever %s: %v", r.id, err)
	}
}

// Output records what the shell printed.
func (r *Recorder) Output(data []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || !r.open(80, 24) {
		return
	}
	buf := append(r.pending, data...)
	r.pending = nil
	// Hold back a partial character at the end: the next chunk completes it.
	for i := 1; i <= utf8.UTFMax-1 && i <= len(buf); i++ {
		tail := buf[len(buf)-i:]
		if utf8.RuneStart(tail[0]) {
			if !utf8.FullRune(tail) {
				r.pending = append([]byte(nil), tail...)
				buf = buf[:len(buf)-i]
			}
			break
		}
	}
	if len(buf) > 0 {
		r.event("o", string(buf))
	}
}

// Resize records a terminal size change; the first one sets the size in the header.
func (r *Recorder) Resize(cols, rows int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	if r.file == nil {
		r.open(cols, rows)
		return
	}
	r.event("r", fmt.Sprintf("%dx%d", cols, rows))
}

// Close ends the recording and stores its final size.
func (r *Recorder) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	if r.file != nil && len(r.pending) > 0 {
		r.event("o", string(r.pending))
		r.pending = nil
	}
	r.closed = true
	if r.file == nil {
		return
	}
	if err := r.file.Close(); err != nil {
		r.m.logger.Printf("[recording] falha ao fechar %s: %v", r.id, err)
	}
	if err := r.m.store.FinishRecording(r.id, time.Now(), r.size, r.truncated); err != nil {
		r.m.logger.Printf("[recording] falha ao finalizar %s: %v", r.id, err)
	}
}
