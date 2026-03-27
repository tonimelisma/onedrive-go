package retry

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-6.8.2
func TestPolicy_Delay_NoJitter(t *testing.T) {
	t.Parallel()

	p := Policy{
		MaxAttempts: 5,
		Base:        1 * time.Second,
		Max:         60 * time.Second,
		Multiplier:  2.0,
		Jitter:      0.0,
	}

	tests := []struct {
		attempt  int
		expected time.Duration
	}{
		{0, 1 * time.Second},
		{1, 2 * time.Second},
		{2, 4 * time.Second},
		{3, 8 * time.Second},
		{4, 16 * time.Second},
		{5, 32 * time.Second},
		{6, 60 * time.Second}, // capped at Max
		{10, 60 * time.Second},
	}

	for _, tt := range tests {
		got := p.Delay(tt.attempt)
		assert.Equal(t, tt.expected, got, "attempt %d", tt.attempt)
	}
}

// Validates: R-6.8.2
func TestPolicy_Delay_WithJitter(t *testing.T) {
	t.Parallel()

	p := Policy{
		MaxAttempts: 5,
		Base:        1 * time.Second,
		Max:         60 * time.Second,
		Multiplier:  2.0,
		Jitter:      0.25,
	}

	// Run many iterations to verify jitter bounds.
	for attempt := range 6 {
		baseDelay := math.Min(float64(p.Base)*math.Pow(p.Multiplier, float64(attempt)), float64(p.Max))
		maxJitter := baseDelay * p.Jitter
		lo := time.Duration(baseDelay - maxJitter)
		hi := time.Duration(baseDelay + maxJitter)

		for range 100 {
			got := p.Delay(attempt)
			require.GreaterOrEqual(t, got, lo, "attempt %d: delay %v below lower bound %v", attempt, got, lo)
			require.LessOrEqual(t, got, hi, "attempt %d: delay %v above upper bound %v", attempt, got, hi)
		}
	}
}

// Validates: R-6.8.2
func TestPolicy_Delay_MaxCap(t *testing.T) {
	t.Parallel()

	p := Policy{
		MaxAttempts: 0,
		Base:        5 * time.Second,
		Max:         30 * time.Second,
		Multiplier:  2.0,
		Jitter:      0.0,
	}

	// 5s * 2^3 = 40s → capped at 30s
	assert.Equal(t, 30*time.Second, p.Delay(3))
	assert.Equal(t, 30*time.Second, p.Delay(10))
}

// Validates: R-6.8.2
// TestNamedPolicies_MatchOriginal verifies named policies match the original constants.
func TestNamedPolicies_MatchOriginal(t *testing.T) {
	t.Parallel()

	// Transport: graph/client.go — maxRetries=5, baseBackoff=1s, maxBackoff=60s, factor=2.0, jitter=0.25
	transport := TransportPolicy()
	assert.Equal(t, 5, transport.MaxAttempts)
	assert.Equal(t, 1*time.Second, transport.Base)
	assert.Equal(t, 60*time.Second, transport.Max)
	assert.InDelta(t, 2.0, transport.Multiplier, 0.0)
	assert.InDelta(t, 0.25, transport.Jitter, 0.0)

	// DriveDiscovery: graph/drives.go — driveDiscoveryRetries=3
	driveDiscovery := DriveDiscoveryPolicy()
	assert.Equal(t, 3, driveDiscovery.MaxAttempts)
	assert.Equal(t, 1*time.Second, driveDiscovery.Base)

	// WatchLocal: observer_local.go — watchErrInitBackoff=1s, watchErrMaxBackoff=30s, watchErrBackoffMult=2
	watchLocal := WatchLocalPolicy()
	assert.Equal(t, 0, watchLocal.MaxAttempts)
	assert.Equal(t, 1*time.Second, watchLocal.Base)
	assert.Equal(t, 30*time.Second, watchLocal.Max)
	assert.InDelta(t, 0.0, watchLocal.Jitter, 0.0)

	// WatchRemote: observer_remote.go — initialWatchBackoff=5s, backoffMultiplier=2
	watchRemote := WatchRemotePolicy()
	assert.Equal(t, 0, watchRemote.MaxAttempts)
	assert.Equal(t, 5*time.Second, watchRemote.Base)
	assert.InDelta(t, 0.0, watchRemote.Jitter, 0.0)
}

// Validates: R-6.8.1, R-6.8.2
// TestTransportDelay_MatchesCalcBackoff verifies Transport.Delay produces the same
// no-jitter base as graph/client.go calcBackoff.
func TestTransportDelay_MatchesCalcBackoff(t *testing.T) {
	t.Parallel()

	// Without jitter, verify base delay values.
	noJitter := Policy{
		MaxAttempts: 5,
		Base:        1 * time.Second,
		Max:         60 * time.Second,
		Multiplier:  2.0,
		Jitter:      0.0,
	}

	expected := []time.Duration{
		1 * time.Second,  // 2^0
		2 * time.Second,  // 2^1
		4 * time.Second,  // 2^2
		8 * time.Second,  // 2^3
		16 * time.Second, // 2^4
	}

	for i, exp := range expected {
		assert.Equal(t, exp, noJitter.Delay(i), "attempt %d", i)
	}
}

// Validates: R-6.8.2
// TestWatchLocalDelay_MatchesOriginal verifies WatchLocal delays.
func TestWatchLocalDelay_MatchesOriginal(t *testing.T) {
	t.Parallel()

	expected := []time.Duration{
		1 * time.Second,  // 1s * 2^0
		2 * time.Second,  // 1s * 2^1
		4 * time.Second,  // 1s * 2^2
		8 * time.Second,  // 1s * 2^3
		16 * time.Second, // 1s * 2^4
		30 * time.Second, // capped
		30 * time.Second, // capped
	}

	for i, exp := range expected {
		assert.Equal(t, exp, WatchLocalPolicy().Delay(i), "attempt %d", i)
	}
}

// Validates: R-6.8.2
// TestWatchRemoteDelay_MatchesOriginal verifies WatchRemote delays.
func TestWatchRemoteDelay_MatchesOriginal(t *testing.T) {
	t.Parallel()

	expected := []time.Duration{
		5 * time.Second,  // 5s * 2^0
		10 * time.Second, // 5s * 2^1
		20 * time.Second, // 5s * 2^2
		40 * time.Second, // 5s * 2^3
		80 * time.Second, // 5s * 2^4
	}

	for i, exp := range expected {
		assert.Equal(t, exp, WatchRemotePolicy().Delay(i), "attempt %d", i)
	}
}
