package retry

import (
	"crypto/rand"
	"encoding/binary"
	mathrand "math/rand/v2"
	"sync"
	"time"
)

// JitterSource is where a retry wait's jitter comes from: a duration drawn
// in [0, maxDelay), or 0 when maxDelay is not positive. Every random choice
// of a wait is drawn from a source its caller passes, never from the global
// math/rand source, so a test that seeds the source replays the waits. The
// sync engine's clock is one (syncdomain.SyncClock.Jitter); a process that
// has no clock of its own seeds a Jitter once at start (NewEntropyJitter).
type JitterSource interface {
	Jitter(maxDelay time.Duration) time.Duration
}

// Jitter is a seeded JitterSource that is safe for concurrent use: the retry
// transport of one process draws from it on every request's goroutine.
type Jitter struct {
	// mu guards rng, which is not safe for concurrent use.
	mu  sync.Mutex
	rng *mathrand.Rand
}

// jitterStreamSalt is the second word of a Jitter's PCG seed.
const jitterStreamSalt = 0x9e3779b97f4a7c15

// NewJitter is the jitter source of seed: two sources of one seed draw the
// same durations in the same order.
func NewJitter(seed uint64) *Jitter {
	return &Jitter{rng: mathrand.New(mathrand.NewPCG(seed, seed^jitterStreamSalt))} //nolint:gosec // jitter does not need crypto rand
}

// NewEntropyJitter is a jitter source seeded once from the operating
// system's random source, for a process or an engine start: two processes
// that retry together do not draw the same waits.
func NewEntropyJitter() *Jitter {
	var seed [8]byte
	_, _ = rand.Read(seed[:]) // crypto/rand.Read never returns an error

	return NewJitter(binary.LittleEndian.Uint64(seed[:]))
}

// Jitter draws a duration in [0, maxDelay).
func (j *Jitter) Jitter(maxDelay time.Duration) time.Duration {
	if maxDelay <= 0 {
		return 0
	}
	j.mu.Lock()
	defer j.mu.Unlock()

	return time.Duration(j.rng.Int64N(int64(maxDelay)))
}
