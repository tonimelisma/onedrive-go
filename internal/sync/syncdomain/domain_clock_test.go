package syncdomain

import (
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// Validates: R-6.10.35
//
// The engine's real clock draws its jitter from a source of its own, seeded
// once when the engine builds it, never from the global math/rand source;
// a clock built without one, as an observer a test builds by hand has, draws
// none.
func TestNewRealClock_DrawsJitterFromItsOwnSource(t *testing.T) {
	t.Parallel()

	const (
		draws    = 100
		maxDelay = time.Minute
	)
	assert.Zero(t, RealClock{}.Jitter(maxDelay), "a clock without a source draws none")

	clock := NewRealClock(slog.New(slog.DiscardHandler), &TimerPanics{})
	drewSome := false
	for range draws {
		got := clock.Jitter(maxDelay)
		assert.GreaterOrEqual(t, got, time.Duration(0))
		assert.Less(t, got, maxDelay)
		drewSome = drewSome || got > 0
	}
	assert.True(t, drewSome, "the engine's clock spreads its waits")
}
