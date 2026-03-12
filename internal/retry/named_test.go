package retry

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// Validates: R-6.8.11
func TestReconcilePolicy(t *testing.T) {
	t.Parallel()

	assert.Equal(t, 1*time.Second, Reconcile.Base, "Reconcile.Base should be 1s")
	assert.Equal(t, 1*time.Hour, Reconcile.Max, "Reconcile.Max should be 1h")
	assert.Equal(t, 2.0, Reconcile.Multiplier, "Reconcile.Multiplier should be 2.0")
	assert.Equal(t, 0.25, Reconcile.Jitter, "Reconcile.Jitter should be 0.25")
	assert.Equal(t, 0, Reconcile.MaxAttempts, "Reconcile.MaxAttempts should be 0 (infinite)")
}

func TestReconcilePolicy_DelayAttemptZero(t *testing.T) {
	t.Parallel()

	delay := Reconcile.Delay(0)
	// With ±25% jitter, delay should be within [0.75s, 1.25s].
	assert.GreaterOrEqual(t, delay, 750*time.Millisecond, "Delay(0) should be >= 0.75s")
	assert.LessOrEqual(t, delay, 1250*time.Millisecond, "Delay(0) should be <= 1.25s")
}

func TestReconcilePolicy_DelayCappedAt1Hour(t *testing.T) {
	t.Parallel()

	// Attempt 12: 2^12 * 1s = 4096s > 3600s cap. Should be capped at 1h ± jitter.
	delay := Reconcile.Delay(12)
	maxWithJitter := time.Duration(float64(time.Hour) * 1.25)
	assert.LessOrEqual(t, delay, maxWithJitter, "Delay(12) should be capped at 1h + jitter")
	assert.Greater(t, delay, 30*time.Minute, "Delay(12) should be > 30min (near 1h cap)")
}
