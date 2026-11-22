package syncsched

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Validates: R-6.8.24
func TestNextReleaseAllowance_RampsFromTheWorkerCountAndDoubles(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		transferWorkers int
		lastAllowance   int
		threshold       int
		want            int
	}{
		{name: "first batch is at least four with a small pool", transferWorkers: 1, lastAllowance: 0, want: 4},
		{name: "first batch is at least four with no pool size", transferWorkers: 0, lastAllowance: 0, want: 4},
		{name: "first batch is the worker count when larger", transferWorkers: 8, lastAllowance: 0, want: 8},
		{name: "first batch ignores the threshold", transferWorkers: 4, lastAllowance: 0, threshold: 16, want: 4},
		{name: "second batch doubles the first", transferWorkers: 4, lastAllowance: 4, want: 8},
		{name: "third batch doubles again", transferWorkers: 4, lastAllowance: 8, want: 16},
		{name: "doubling continues past the worker count", transferWorkers: 8, lastAllowance: 64, want: 128},
		{name: "doubling stops at the threshold", transferWorkers: 4, lastAllowance: 8, threshold: 16, want: 16},
		{name: "doubling lands on the threshold when it would pass it", transferWorkers: 4, lastAllowance: 8, threshold: 12, want: 12},
		{name: "above the threshold the batch grows by one", transferWorkers: 4, lastAllowance: 16, threshold: 16, want: 17},
		{name: "additive growth continues", transferWorkers: 4, lastAllowance: 17, threshold: 16, want: 18},
		{name: "the cap holds", transferWorkers: 4, lastAllowance: releaseRampMaxBatch, want: releaseRampMaxBatch},
		{name: "the last doubling lands on the cap", transferWorkers: 4, lastAllowance: releaseRampMaxBatch / 2, want: releaseRampMaxBatch},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, NextReleaseAllowance(tc.transferWorkers, tc.lastAllowance, tc.threshold))
		})
	}
}

// Validates: R-6.8.24
func TestSlowStartThresholdAfterThrottle_RemembersHalfTheBatchThatDrewThe429(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                string
		transferWorkers     int
		allowanceAtThrottle int
		previous            int
		want                int
	}{
		{name: "half the batch", transferWorkers: 4, allowanceAtThrottle: 32, want: 16},
		{name: "never below the first batch", transferWorkers: 4, allowanceAtThrottle: 6, want: 4},
		{name: "the first batch follows the worker count", transferWorkers: 8, allowanceAtThrottle: 6, want: 8},
		{name: "a 429 outside a ramp keeps the previous threshold", transferWorkers: 4, allowanceAtThrottle: 0, previous: 16, want: 16},
		{name: "a 429 outside a ramp with no memory leaves none", transferWorkers: 4, allowanceAtThrottle: 0, want: 0},
		{name: "a later 429 replaces the memory", transferWorkers: 4, allowanceAtThrottle: 18, previous: 16, want: 9},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, SlowStartThresholdAfterThrottle(tc.transferWorkers, tc.allowanceAtThrottle, tc.previous))
		})
	}
}
