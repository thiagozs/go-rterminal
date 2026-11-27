package live

import (
	"bytes"
	"testing"
)

func next(t *testing.T, w *Watcher) Event {
	t.Helper()
	select {
	case ev, ok := <-w.Events:
		if !ok {
			t.Fatal("channel closed")
		}
		return ev
	default:
		t.Fatal("no event waiting")
	}
	return Event{}
}

func TestSessionIsListedOnlyOnceItHasTraffic(t *testing.T) {
	h := NewHub()
	s := h.Begin("", "m1", "pi", "root")
	if len(h.List(nil)) != 0 {
		t.Fatal("listed before any traffic")
	}
	s.Resize(80, 24)
	if len(h.List(nil)) != 0 {
		t.Fatal("listed without an id")
	}
	s.SetID("sess-1")
	list := h.List(nil)
	if len(list) != 1 || list[0].ID != "sess-1" || list[0].StartedAt.IsZero() {
		t.Fatalf("list = %+v", list)
	}
	if got := h.List(func(id string) bool { return id == "other" }); len(got) != 0 {
		t.Fatalf("filter ignored: %+v", got)
	}
	s.Close()
	if len(h.List(nil)) != 0 {
		t.Fatal("still listed after Close")
	}
	if _, _, ok := h.Watch("sess-1"); ok {
		t.Fatal("watching a closed session")
	}
}

func TestWatcherGetsReplayThenLive(t *testing.T) {
	h := NewHub()
	s := h.Begin("sess", "m1", "pi", "root")
	s.Resize(100, 30)
	s.Output([]byte("old"))
	w, info, ok := h.Watch("sess")
	if !ok || info.Watchers != 1 {
		t.Fatalf("watch: ok=%v info=%+v", ok, info)
	}
	if ev := next(t, w); ev.Kind != "resize" || ev.Cols != 100 || ev.Rows != 30 {
		t.Fatalf("first = %+v", ev)
	}
	if ev := next(t, w); ev.Kind != "output" || string(ev.Data) != "old" {
		t.Fatalf("replay = %+v", ev)
	}
	s.Output([]byte("new"))
	if ev := next(t, w); string(ev.Data) != "new" {
		t.Fatalf("live = %+v", ev)
	}
	s.Close()
	if ev := next(t, w); ev.Kind != "end" {
		t.Fatalf("end = %+v", ev)
	}
	w.Stop() // after the end: must not panic
	w.Stop()
}

func TestReplayIsBounded(t *testing.T) {
	h := NewHub()
	s := h.Begin("sess", "m", "pi", "root")
	chunk := bytes.Repeat([]byte("x"), 64<<10)
	for i := 0; i < 10; i++ {
		s.Output(chunk)
	}
	s.Output([]byte("fim"))
	w, _, _ := h.Watch("sess")
	ev := next(t, w)
	if len(ev.Data) > replayBytes || !bytes.HasSuffix(ev.Data, []byte("fim")) {
		t.Fatalf("replay len=%d", len(ev.Data))
	}
}

func TestSlowWatcherIsDroppedWithoutBlocking(t *testing.T) {
	h := NewHub()
	s := h.Begin("sess", "m", "pi", "root")
	s.Output([]byte("a"))
	slow, _, _ := h.Watch("sess")
	fast, _, _ := h.Watch("sess")
	for i := 0; i < watcherQueue*2; i++ {
		s.Output([]byte("b"))
		// The fast watcher keeps up.
		for len(fast.Events) > 0 {
			<-fast.Events
		}
	}
	var last Event
	for ev := range slow.Events {
		last = ev
	}
	if last.Kind != "end" || string(last.Data) != "too slow" {
		t.Fatalf("slow watcher ended with %+v", last)
	}
	if got := h.List(nil)[0].Watchers; got != 1 {
		t.Fatalf("watchers = %d, want only the fast one", got)
	}
	s.Close()
}

func TestStoppedWatcherReceivesNothing(t *testing.T) {
	h := NewHub()
	s := h.Begin("sess", "m", "pi", "root")
	s.Output([]byte("a"))
	w, _, _ := h.Watch("sess")
	w.Stop()
	s.Output([]byte("b"))
	s.Close()
	for ev := range drain(w) {
		if string(ev.Data) == "b" {
			t.Fatal("stopped watcher got output")
		}
	}
}

func drain(w *Watcher) <-chan Event {
	out := make(chan Event, 16)
	for {
		select {
		case ev := <-w.Events:
			out <- ev
		default:
			close(out)
			return out
		}
	}
}
