package syncsched

import "time"

// Re-admission after a timed block scope ramps instead of flooding
// (R-6.8.24). Measured on a live account on 2026-11-01: a successful trial
// released a 10,000-upload backlog at once, the workers drained it through
// 429 after 429, and the next trial repeated the flood -- 22-28 requests a
// second, thousands of 429s per five minutes, about ten files uploaded. The
// ramp bounds what one trial lets through, and a 429 inside it re-blocks.

const (
	// releaseRampMinimumBatch is the first batch after a successful trial
	// when fewer transfer workers are configured: enough to keep a small
	// pool busy, few enough that a 429 costs little.
	releaseRampMinimumBatch = 4

	// releaseRampMaxBatch caps the doubling. In practice a batch this size
	// is the whole backlog; the cap only keeps the arithmetic bounded.
	releaseRampMaxBatch = 1 << 16

	// ReleaseBatchAdmissionWindow is how far a released batch's members are
	// spread: each is admitted at a uniformly random instant within this
	// window of the release, through the engine clock, so a batch of 32
	// does not reach the drive as one spike (R-6.8.24; Google SRE's
	// randomized retries, Brooker's jitter). The server-given Retry-After
	// gets no jitter: the trial that honors it is one request.
	ReleaseBatchAdmissionWindow = time.Second
)

// NextReleaseAllowance is the ramp policy: the size of the next batch a scope
// on probation releases, given the transfer worker count, the batch it last
// released (zero when the scope is not on probation yet) and the slow-start
// threshold its last 429 left (zero when none has). The first batch is the
// worker count, at least releaseRampMinimumBatch; each batch that completed
// without a further 429 doubles the next, up to the threshold; from there
// the batch grows by one. Pure doubling overshoots the limit by up to 2x on
// every cycle, and each overshoot costs a full Retry-After plus Microsoft's
// penalty for requests made while throttled (RFC 5681 §3.1 eq. 4 is the
// same remedy).
func NextReleaseAllowance(transferWorkers, lastAllowance, slowStartThreshold int) int {
	if lastAllowance <= 0 {
		return FirstReleaseAllowance(transferWorkers)
	}
	if lastAllowance >= releaseRampMaxBatch {
		return releaseRampMaxBatch
	}
	doubled := min(lastAllowance*2, releaseRampMaxBatch)
	if slowStartThreshold <= 0 || doubled <= slowStartThreshold {
		return doubled
	}
	if lastAllowance < slowStartThreshold {
		return slowStartThreshold
	}

	return lastAllowance + 1
}

// FirstReleaseAllowance is the first batch a successful trial releases.
func FirstReleaseAllowance(transferWorkers int) int {
	return max(transferWorkers, releaseRampMinimumBatch)
}

// SlowStartThresholdAfterThrottle is the ramp's memory after a 429: half the
// batch that drew it, at least the first batch. A 429 that interrupted no
// ramp (the probe itself, or an episode's first failures) says nothing new
// and keeps the previous threshold.
func SlowStartThresholdAfterThrottle(transferWorkers, allowanceAtThrottle, previous int) int {
	if allowanceAtThrottle <= 0 {
		return previous
	}

	return max(allowanceAtThrottle/2, FirstReleaseAllowance(transferWorkers))
}
