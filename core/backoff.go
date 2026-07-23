package core

import (
	"math/rand"
	"time"
)

// backoff implements independent exponential-backoff-with-jitter per
// failure mode (draft/micro-machine.md §7): enrollment, heartbeat,
// long-poll, and RTB session reconnect each get their own instance —
// a failure on one path never throttles the others.
type backoff struct {
	min, max time.Duration
	current  time.Duration
}

func newBackoff(min, max time.Duration) *backoff {
	return &backoff{min: min, max: max, current: min}
}

// Next returns the delay to wait before the next attempt, doubling from
// the previous one (capped at max) and jittering ±25%.
func (b *backoff) Next() time.Duration {
	d := b.current
	b.current *= 2
	if b.current > b.max {
		b.current = b.max
	}
	jitter := time.Duration(rand.Int63n(int64(d)/2+1)) - d/4
	d += jitter
	if d < b.min {
		d = b.min
	}
	return d
}

// Reset is called after any successful attempt — critically, an ordinary
// empty long-poll return is a *successful* call on this path, not a
// failure (draft/micro-machine.md §7).
func (b *backoff) Reset() {
	b.current = b.min
}
