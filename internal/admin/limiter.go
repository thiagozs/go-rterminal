package admin

import (
	"sync"
	"time"
)

// loginLimiter blocks a key after max failures within window.
type loginLimiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	hits   map[string][]time.Time
}

func newLoginLimiter(max int, window time.Duration) *loginLimiter {
	return &loginLimiter{max: max, window: window, hits: make(map[string][]time.Time)}
}

func (l *loginLimiter) recent(key string, now time.Time) []time.Time {
	kept := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if now.Sub(t) < l.window {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(l.hits, key)
		return nil
	}
	l.hits[key] = kept
	return kept
}

func (l *loginLimiter) allow(key string) bool {
	blocked, _ := l.blocked(key)
	return !blocked
}

// blocked reports whether key has reached its failure limit and, if so, how
// long until the oldest counted failure expires and an attempt is allowed again.
func (l *loginLimiter) blocked(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	recent := l.recent(key, now)
	if len(recent) < l.max {
		return false, 0
	}
	// recent is oldest-first; once the (len-max+1)-th oldest ages out, the count drops below max.
	return true, recent[len(recent)-l.max].Add(l.window).Sub(now)
}

func (l *loginLimiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	l.hits[key] = append(l.recent(key, now), now)
}

func (l *loginLimiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.hits, key)
}
