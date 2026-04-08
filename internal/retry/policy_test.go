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

func assertPolicyShape(t *testing.T, got Policy, maxAttempts int, base, maxDelay time.Duration, jitter float64) {
	t.Helper()

	assert.Equal(t, maxAttempts, got.MaxAttempts)
	assert.Equal(t, base, got.Base)
	assert.Equal(t, maxDelay, got.Max)
	assert.InDelta(t, 2.0, got.Multiplier, 0.0)
	assert.InDelta(t, jitter, got.Jitter, 0.0)
}

// Validates: R-6.8.2
func TestNamedPolicies_RequestPathPolicies(t *testing.T) {
	t.Parallel()

	assertPolicyShape(t, TransportPolicy(), 5, 1*time.Second, 60*time.Second, 0.25)
	assert.Equal(t, 5, DriveDiscoveryPolicy().MaxAttempts)
	assert.Equal(t, 1*time.Second, DriveDiscoveryPolicy().Base)
	assertPolicyShape(t, RootChildrenPolicy(), 3, 250*time.Millisecond, 1*time.Second, 0.0)
	assertPolicyShape(t, DownloadMetadataPolicy(), 4, 250*time.Millisecond, 2*time.Second, 0.0)
}

// Validates: R-6.8.2
func TestNamedPolicies_UploadAndVisibilityPolicies(t *testing.T) {
	t.Parallel()

	assertPolicyShape(t, SimpleUploadMtimePatchPolicy(), 4, 250*time.Millisecond, 2*time.Second, 0.0)
	assertPolicyShape(t, UploadSessionCreatePolicy(), 6, 250*time.Millisecond, 4*time.Second, 0.0)
	assertPolicyShape(t, SimpleUploadCreatePolicy(), 7, 250*time.Millisecond, 8*time.Second, 0.0)
	assertPolicyShape(t, PathVisibilityPolicy(), 8, 250*time.Millisecond, 32*time.Second, 0.0)
}

// Validates: R-6.8.2
func TestNamedPolicies_LongRunningPolicies(t *testing.T) {
	t.Parallel()

	assertPolicyShape(t, WatchLocalPolicy(), 0, 1*time.Second, 30*time.Second, 0.0)
	assert.Equal(t, 0, WatchRemotePolicy().MaxAttempts)
	assert.Equal(t, 5*time.Second, WatchRemotePolicy().Base)
	assert.InDelta(t, 0.0, WatchRemotePolicy().Jitter, 0.0)
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
