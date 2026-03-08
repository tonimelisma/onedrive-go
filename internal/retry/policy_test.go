package retry

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

// TestNamedPolicies_MatchOriginal verifies named policies match the original constants.
func TestNamedPolicies_MatchOriginal(t *testing.T) {
	t.Parallel()

	// Transport: graph/client.go — maxRetries=5, baseBackoff=1s, maxBackoff=60s, factor=2.0, jitter=0.25
	assert.Equal(t, 5, Transport.MaxAttempts)
	assert.Equal(t, 1*time.Second, Transport.Base)
	assert.Equal(t, 60*time.Second, Transport.Max)
	assert.Equal(t, 2.0, Transport.Multiplier)
	assert.Equal(t, 0.25, Transport.Jitter)

	// DriveDiscovery: graph/drives.go — driveDiscoveryRetries=3
	assert.Equal(t, 3, DriveDiscovery.MaxAttempts)
	assert.Equal(t, 1*time.Second, DriveDiscovery.Base)

	// Action: sync/executor.go — executorMaxRetries=3, executorBaseDelay=1s, executorBackoffExp=2.0, executorJitter=0.25
	assert.Equal(t, 3, Action.MaxAttempts)
	assert.Equal(t, 1*time.Second, Action.Base)
	assert.Equal(t, 2.0, Action.Multiplier)
	assert.Equal(t, 0.25, Action.Jitter)

	// Reconcile: sync/baseline.go — infinite retries, 30s base, 1h max, 25% jitter
	assert.Equal(t, 0, Reconcile.MaxAttempts)
	assert.Equal(t, 30*time.Second, Reconcile.Base)
	assert.Equal(t, 1*time.Hour, Reconcile.Max)
	assert.Equal(t, 0.25, Reconcile.Jitter)

	// WatchLocal: observer_local.go — watchErrInitBackoff=1s, watchErrMaxBackoff=30s, watchErrBackoffMult=2
	assert.Equal(t, 0, WatchLocal.MaxAttempts)
	assert.Equal(t, 1*time.Second, WatchLocal.Base)
	assert.Equal(t, 30*time.Second, WatchLocal.Max)
	assert.Equal(t, 0.0, WatchLocal.Jitter)

	// WatchRemote: observer_remote.go — initialWatchBackoff=5s, backoffMultiplier=2
	assert.Equal(t, 0, WatchRemote.MaxAttempts)
	assert.Equal(t, 5*time.Second, WatchRemote.Base)
	assert.Equal(t, 0.0, WatchRemote.Jitter)
}

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

// TestReconcileDelay_MatchesComputeNextRetry verifies the Reconcile policy
// produces delays matching the original computeNextRetry in baseline.go.
// Original: delaySec = min(30*(1<<failureCount), 3600)
func TestReconcileDelay_MatchesComputeNextRetry(t *testing.T) {
	t.Parallel()

	noJitter := Policy{
		MaxAttempts: 0,
		Base:        30 * time.Second,
		Max:         1 * time.Hour,
		Multiplier:  2.0,
		Jitter:      0.0,
	}

	// Original computeNextRetry used bit shift: 30 * (1 << failureCount)
	// Policy.Delay uses: 30s * 2^attempt
	// These are mathematically identical.
	tests := []struct {
		attempt  int
		expected time.Duration
	}{
		{0, 30 * time.Second},      // 30 * 1
		{1, 60 * time.Second},      // 30 * 2
		{2, 120 * time.Second},     // 30 * 4
		{3, 240 * time.Second},     // 30 * 8
		{4, 480 * time.Second},     // 30 * 16
		{5, 960 * time.Second},     // 30 * 32
		{6, 1920 * time.Second},    // 30 * 64
		{7, 3600 * time.Second},    // capped at 1h (30 * 128 = 3840 > 3600)
		{8, 3600 * time.Second},    // still capped
		{9, time.Duration(3600e9)}, // still capped
	}

	for _, tt := range tests {
		got := noJitter.Delay(tt.attempt)
		assert.Equal(t, tt.expected, got, "attempt %d", tt.attempt)
	}
}

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
		assert.Equal(t, exp, WatchLocal.Delay(i), "attempt %d", i)
	}
}

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
		assert.Equal(t, exp, WatchRemote.Delay(i), "attempt %d", i)
	}
}
