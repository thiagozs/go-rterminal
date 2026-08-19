package backoff

import (
	"math/rand"
	"time"
)

type State struct {
	min time.Duration
	max time.Duration
	cur time.Duration
	rng *rand.Rand
}

func New(min, max time.Duration, rng *rand.Rand) *State {
	return &State{min: min, max: max, cur: min, rng: rng}
}

func (b *State) Next() time.Duration {
	limit := b.cur
	if limit > b.max {
		limit = b.max
	}
	delay := time.Duration(b.rng.Int63n(int64(limit) + 1))
	if b.cur < b.max/2 {
		b.cur *= 2
	} else {
		b.cur = b.max
	}
	return delay
}

func (b *State) Reset() {
	b.cur = b.min
}
