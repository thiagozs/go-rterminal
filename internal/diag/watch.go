package diag

import (
	"context"
	"fmt"
	"time"
)

const (
	watchInterval  = 5 * time.Second
	suspendMinimum = 15 * time.Second
)

// suspendedFor tells how long the machine was asleep given how much the wall
// clock and the monotonic clock advanced between two ticks. The monotonic
// clock stops while the machine sleeps; the wall clock does not.
func suspendedFor(wall, mono time.Duration) (time.Duration, bool) {
	gap := wall - mono
	return gap, gap >= suspendMinimum
}

// WatchSuspend records a "suspend" event whenever the machine slept (or was
// frozen) for a while. It returns when ctx ends.
func WatchSuspend(ctx context.Context, j *Journal) {
	last := time.Now()
	ticker := time.NewTicker(watchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now()
			wall := now.Round(0).Sub(last.Round(0)) // Round(0) drops the monotonic reading
			mono := now.Sub(last)
			if gap, slept := suspendedFor(wall, mono); slept {
				j.Record("suspend", fmt.Sprintf("a máquina ficou cerca de %s suspensa (dormiu ou congelou); a conexão com o servidor cai nesse período", gap.Round(time.Second)))
			}
			last = now
		}
	}
}
