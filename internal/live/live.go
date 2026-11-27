// Package live lets other people watch a web terminal session while it runs.
//
// A Hub keeps the sessions that are in progress. Each one is a relay.Tap, so
// it sees what the shell prints (never what the user types) and the terminal
// size. Watchers get a short replay of the latest output followed by the live
// stream; a watcher that cannot keep up is dropped instead of slowing the
// session down.
package live

import (
	"sort"
	"sync"
	"time"
)

const (
	// replayBytes is how much recent output a new watcher receives first.
	replayBytes = 128 << 10
	// watcherQueue is how many chunks may wait for a slow watcher.
	watcherQueue = 256
)

// Event is what a watcher receives.
type Event struct {
	Kind       string // "output", "resize" or "end"
	Data       []byte // output bytes
	Cols, Rows int    // resize
}

// Info describes a running session.
type Info struct {
	ID          string    `json:"id"`
	MachineID   string    `json:"machine_id"`
	MachineName string    `json:"machine_name"`
	Username    string    `json:"username"`
	StartedAt   time.Time `json:"started_at"`
	Watchers    int       `json:"watchers"`
}

type Hub struct {
	mu       sync.Mutex
	sessions map[string]*Session
}

func NewHub() *Hub { return &Hub{sessions: make(map[string]*Session)} }

// Session is one running terminal. It implements relay.Tap.
type Session struct {
	hub  *Hub
	info Info

	mu       sync.Mutex
	started  bool // saw traffic; listed once it also has an id
	listed   bool
	closed   bool
	cols     int
	rows     int
	replay   []byte
	watchers map[*Watcher]struct{}
}

// Watcher is one spectator of a Session.
type Watcher struct {
	Events <-chan Event
	ch     chan Event
	sess   *Session
	once   sync.Once
}

// Begin prepares a session. It only becomes visible once it prints or resizes
// for the first time, so a session that never starts leaves nothing behind.
func (h *Hub) Begin(id, machineID, machineName, username string) *Session {
	return &Session{
		hub:      h,
		info:     Info{ID: id, MachineID: machineID, MachineName: machineName, Username: username},
		watchers: make(map[*Watcher]struct{}),
	}
}

// SetID names the session once the relay has assigned its id.
func (s *Session) SetID(id string) {
	s.mu.Lock()
	s.info.ID = id
	if s.started {
		s.list()
	}
	s.mu.Unlock()
}

// list makes the session visible. s.mu must be held.
func (s *Session) list() {
	s.started = true
	if s.listed || s.closed || s.info.ID == "" {
		return
	}
	s.listed = true
	s.info.StartedAt = time.Now().UTC()
	s.hub.mu.Lock()
	s.hub.sessions[s.info.ID] = s
	s.hub.mu.Unlock()
}

func (s *Session) Output(data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.list()
	s.replay = append(s.replay, data...)
	if over := len(s.replay) - replayBytes; over > 0 {
		s.replay = append([]byte(nil), s.replay[over:]...)
	}
	chunk := append([]byte(nil), data...)
	s.broadcast(Event{Kind: "output", Data: chunk})
}

func (s *Session) Resize(cols, rows int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.list()
	s.cols, s.rows = cols, rows
	s.broadcast(Event{Kind: "resize", Cols: cols, Rows: rows})
}

// Close ends the session and tells every watcher.
func (s *Session) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	listed := s.listed
	ws := s.watchers
	s.watchers = nil
	s.mu.Unlock()
	if listed {
		s.hub.mu.Lock()
		delete(s.hub.sessions, s.info.ID)
		s.hub.mu.Unlock()
	}
	for w := range ws {
		w.ch <- Event{Kind: "end"} // room is reserved: see broadcast
		close(w.ch)
	}
}

// broadcast hands ev to every watcher, dropping the ones that fell behind.
// One slot of each queue stays free for the final "end". s.mu must be held.
func (s *Session) broadcast(ev Event) {
	for w := range s.watchers {
		if len(w.ch) >= watcherQueue-1 {
			delete(s.watchers, w)
			w.ch <- Event{Kind: "end", Data: []byte("too slow")}
			close(w.ch)
			continue
		}
		w.ch <- ev
	}
}

// Watch starts watching session id. The first events are the current size and
// the recent output. ok is false when the session does not exist (anymore).
func (h *Hub) Watch(id string) (w *Watcher, info Info, ok bool) {
	h.mu.Lock()
	s := h.sessions[id]
	h.mu.Unlock()
	if s == nil {
		return nil, Info{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, Info{}, false
	}
	ch := make(chan Event, watcherQueue)
	w = &Watcher{Events: ch, ch: ch, sess: s}
	if s.cols > 0 && s.rows > 0 {
		ch <- Event{Kind: "resize", Cols: s.cols, Rows: s.rows}
	}
	if len(s.replay) > 0 {
		ch <- Event{Kind: "output", Data: append([]byte(nil), s.replay...)}
	}
	s.watchers[w] = struct{}{}
	info = s.info
	info.Watchers = len(s.watchers)
	return w, info, true
}

// Stop ends the watch. It is safe to call more than once and after the
// session ended.
func (w *Watcher) Stop() {
	w.once.Do(func() {
		w.sess.mu.Lock()
		delete(w.sess.watchers, w)
		w.sess.mu.Unlock()
	})
}

// List returns the running sessions, newest first. visible, when not nil,
// filters by machine id.
func (h *Hub) List(visible func(machineID string) bool) []Info {
	h.mu.Lock()
	sessions := make([]*Session, 0, len(h.sessions))
	for _, s := range h.sessions {
		sessions = append(sessions, s)
	}
	h.mu.Unlock()
	out := make([]Info, 0, len(sessions))
	for _, s := range sessions {
		s.mu.Lock()
		info := s.info
		info.Watchers = len(s.watchers)
		s.mu.Unlock()
		if visible == nil || visible(info.MachineID) {
			out = append(out, info)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	return out
}
