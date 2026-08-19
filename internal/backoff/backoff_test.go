package backoff

import (
	"math/rand"
	"testing"
	"time"
)

func TestStateBoundsAndReset(t *testing.T) {
	t.Parallel()
	state := New(10*time.Millisecond, 40*time.Millisecond, rand.New(rand.NewSource(1)))
	limits := []time.Duration{10, 20, 40, 40}
	for i, limit := range limits {
		delay := state.Next()
		if delay < 0 || delay > limit*time.Millisecond {
			t.Fatalf("Next() call %d = %s, want [0, %s]", i, delay, limit*time.Millisecond)
		}
	}
	state.Reset()
	if delay := state.Next(); delay > 10*time.Millisecond {
		t.Fatalf("Next() after Reset = %s, want <= 10ms", delay)
	}
}
