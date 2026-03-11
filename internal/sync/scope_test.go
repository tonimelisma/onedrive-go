package sync

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// discardLogger returns a logger that writes to nowhere, suitable for tests.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// controllableClock returns a nowFunc and a function to advance the clock.
// The clock starts at a fixed epoch to keep tests deterministic.
func controllableClock() (nowFunc func() time.Time, advance func(d time.Duration)) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	return func() time.Time { return now },
		func(d time.Duration) { now = now.Add(d) }
}

// Validates: R-2.10.3, R-2.10.26
func TestScope_429Immediate(t *testing.T) {
	t.Parallel()

	clock, _ := controllableClock()
	ss := NewScopeState(clock, discardLogger())

	// A single 429 must immediately trigger a throttle:account block.
	r := WorkerResult{
		Path:       "/file-a.txt",
		HTTPStatus: 429,
		RetryAfter: 90 * time.Second,
	}
	result := ss.UpdateScope(&r)

	require.True(t, result.Block, "single 429 must trigger immediate block")
	assert.Equal(t, "throttle:account", result.ScopeKey)
	assert.Equal(t, "rate_limited", result.IssueType)
	assert.Equal(t, 90*time.Second, result.TrialInterval, "trial interval should honor Retry-After")
}

// Validates: R-2.10.3, R-2.10.26
func TestScope_429FallbackInterval(t *testing.T) {
	t.Parallel()

	clock, _ := controllableClock()
	ss := NewScopeState(clock, discardLogger())

	// 429 without Retry-After should use the serviceInitialInterval fallback.
	r := WorkerResult{
		Path:       "/file-a.txt",
		HTTPStatus: 429,
	}
	result := ss.UpdateScope(&r)

	require.True(t, result.Block)
	assert.Equal(t, "throttle:account", result.ScopeKey)
	assert.Equal(t, serviceInitialInterval, result.TrialInterval, "429 without Retry-After must fall back to serviceInitialInterval")
}

// Validates: R-2.10.3
func TestScope_503WithRetryAfter(t *testing.T) {
	t.Parallel()

	clock, _ := controllableClock()
	ss := NewScopeState(clock, discardLogger())

	// A 503 with Retry-After must immediately trigger a service block.
	r := WorkerResult{
		Path:       "/doc.docx",
		HTTPStatus: 503,
		RetryAfter: 120 * time.Second,
	}
	result := ss.UpdateScope(&r)

	require.True(t, result.Block, "503 with Retry-After must trigger immediate service block")
	assert.Equal(t, "service", result.ScopeKey)
	assert.Equal(t, "service_outage", result.IssueType)
	assert.Equal(t, 120*time.Second, result.TrialInterval, "trial interval should use Retry-After value")
}

// Validates: R-2.10.3
func TestScope_503WithoutRetryAfter(t *testing.T) {
	t.Parallel()

	clock, _ := controllableClock()
	ss := NewScopeState(clock, discardLogger())

	// A 503 without Retry-After should feed the service sliding window,
	// not trigger an immediate block (it falls into the >= 500 case).
	r := WorkerResult{
		Path:       "/doc.docx",
		HTTPStatus: 503,
	}
	result := ss.UpdateScope(&r)

	assert.False(t, result.Block, "503 without Retry-After must NOT immediately block; it feeds the sliding window")
}

// Validates: R-2.10.1, R-2.10.3, R-2.10.19
func TestScope_507OwnDrive(t *testing.T) {
	t.Parallel()

	clock, advance := controllableClock()
	ss := NewScopeState(clock, discardLogger())

	// Three unique paths within the quota window should trigger quota:own.
	paths := []string{"/a.txt", "/b.txt", "/c.txt"}
	for i, p := range paths {
		r := WorkerResult{
			Path:       p,
			HTTPStatus: 507,
		}
		result := ss.UpdateScope(&r)
		if i < len(paths)-1 {
			assert.False(t, result.Block, "path %d (%s) should not trigger block yet", i, p)
		} else {
			require.True(t, result.Block, "third unique path must trigger quota:own block")
			assert.Equal(t, "quota:own", result.ScopeKey)
			assert.Equal(t, "quota_exceeded", result.IssueType)
			assert.Equal(t, quotaInitialInterval, result.TrialInterval)
		}
		advance(1 * time.Second) // stay well within the 10s window
	}
}

// Validates: R-2.10.3, R-2.10.17, R-2.10.20
func TestScope_507Shortcut(t *testing.T) {
	t.Parallel()

	clock, advance := controllableClock()
	ss := NewScopeState(clock, discardLogger())

	shortcutKey := "abc123:item456"
	paths := []string{"/shared/x.doc", "/shared/y.doc", "/shared/z.doc"}
	for i, p := range paths {
		r := WorkerResult{
			Path:        p,
			HTTPStatus:  507,
			ShortcutKey: shortcutKey,
		}
		result := ss.UpdateScope(&r)
		if i < len(paths)-1 {
			assert.False(t, result.Block, "path %d should not trigger block yet", i)
		} else {
			require.True(t, result.Block, "third unique shortcut path must trigger quota:shortcut block")
			assert.Equal(t, "quota:shortcut:"+shortcutKey, result.ScopeKey)
			assert.Equal(t, "quota_exceeded", result.IssueType)
			assert.Equal(t, quotaInitialInterval, result.TrialInterval)
		}
		advance(1 * time.Second)
	}
}

// Validates: R-2.10.39
func TestScope_507IndependentShortcuts(t *testing.T) {
	t.Parallel()

	clock, advance := controllableClock()
	ss := NewScopeState(clock, discardLogger())

	keyA := "driveA:itemA"
	keyB := "driveB:itemB"

	// Feed two failures on shortcut A — not enough to trigger.
	for _, p := range []string{"/shared-a/1.txt", "/shared-a/2.txt"} {
		r := WorkerResult{Path: p, HTTPStatus: 507, ShortcutKey: keyA}
		result := ss.UpdateScope(&r)
		assert.False(t, result.Block)
		advance(1 * time.Second)
	}

	// Feed three failures on shortcut B — should trigger block for B only.
	for i, p := range []string{"/shared-b/1.txt", "/shared-b/2.txt", "/shared-b/3.txt"} {
		r := WorkerResult{Path: p, HTTPStatus: 507, ShortcutKey: keyB}
		result := ss.UpdateScope(&r)
		if i < 2 {
			assert.False(t, result.Block)
		} else {
			require.True(t, result.Block, "shortcut B must trigger independently")
			assert.Equal(t, "quota:shortcut:"+keyB, result.ScopeKey)
		}
		advance(1 * time.Second)
	}

	// One more failure on shortcut A — still at 2 unique paths after B
	// triggered, so A must not block (only 3 unique paths needed but we
	// only have 2 for A).
	rA3 := WorkerResult{Path: "/shared-a/3.txt", HTTPStatus: 507, ShortcutKey: keyA}
	resultA3 := ss.UpdateScope(&rA3)
	require.True(t, resultA3.Block, "third unique path on shortcut A should now trigger")
	assert.Equal(t, "quota:shortcut:"+keyA, resultA3.ScopeKey,
		"shortcut A block must be independent from shortcut B")
}

// Validates: R-2.10.3, R-2.10.28, R-2.10.29
func TestScope_5xxSlidingWindow(t *testing.T) {
	t.Parallel()

	clock, advance := controllableClock()
	ss := NewScopeState(clock, discardLogger())

	// Five unique paths with 5xx within 30s must trigger a service block.
	paths := []string{"/a.txt", "/b.txt", "/c.txt", "/d.txt", "/e.txt"}
	for i, p := range paths {
		r := WorkerResult{
			Path:       p,
			HTTPStatus: 500,
		}
		result := ss.UpdateScope(&r)
		if i < len(paths)-1 {
			assert.False(t, result.Block, "path %d should not trigger block yet", i)
		} else {
			require.True(t, result.Block, "fifth unique path must trigger service block")
			assert.Equal(t, "service", result.ScopeKey)
			assert.Equal(t, "service_outage", result.IssueType)
			assert.Equal(t, serviceInitialInterval, result.TrialInterval)
		}
		advance(2 * time.Second) // total 8s, well within 30s window
	}
}

// Validates: R-2.10.3, R-2.10.28, R-2.10.29
func TestScope_5xxWindowExpiry(t *testing.T) {
	t.Parallel()

	clock, advance := controllableClock()
	ss := NewScopeState(clock, discardLogger())

	// Feed four failures then wait for the window to expire. The fifth
	// failure after expiry must NOT trigger a block because the earlier
	// entries have aged out.
	for _, p := range []string{"/a.txt", "/b.txt", "/c.txt", "/d.txt"} {
		r := WorkerResult{Path: p, HTTPStatus: 500}
		result := ss.UpdateScope(&r)
		assert.False(t, result.Block)
		advance(1 * time.Second)
	}

	// Advance past the 30s window so the first entries expire.
	advance(30 * time.Second)

	r := WorkerResult{Path: "/e.txt", HTTPStatus: 500}
	result := ss.UpdateScope(&r)
	assert.False(t, result.Block, "entries from before the window should have expired")
}

// Validates: R-2.10.3, R-2.10.42
func TestScope_SuccessResetsWindow(t *testing.T) {
	t.Parallel()

	clock, advance := controllableClock()
	ss := NewScopeState(clock, discardLogger())

	// Accumulate two 507 failures on own drive.
	for _, p := range []string{"/a.txt", "/b.txt"} {
		r := WorkerResult{Path: p, HTTPStatus: 507}
		result := ss.UpdateScope(&r)
		assert.False(t, result.Block)
		advance(1 * time.Second)
	}

	// Record a success — this must reset the quota:own window.
	ss.RecordSuccess(&WorkerResult{Path: "/ok.txt"})

	// Now three more unique failures are needed to trigger the block.
	// The next failure (3rd unique path overall but 1st after reset)
	// must NOT trigger.
	for i, p := range []string{"/c.txt", "/d.txt"} {
		r := WorkerResult{Path: p, HTTPStatus: 507}
		result := ss.UpdateScope(&r)
		assert.False(t, result.Block, "after reset, path %d should not trigger", i)
		advance(1 * time.Second)
	}

	// Third unique path after reset should trigger.
	r := WorkerResult{Path: "/e.txt", HTTPStatus: 507}
	result := ss.UpdateScope(&r)
	require.True(t, result.Block, "third unique path after reset should trigger quota:own")
	assert.Equal(t, "quota:own", result.ScopeKey)
}

// Validates: R-2.10.42
func TestScope_SuccessResetsShortcutWindow(t *testing.T) {
	t.Parallel()

	clock, advance := controllableClock()
	ss := NewScopeState(clock, discardLogger())

	shortcutKey := "driveX:itemX"

	// Two failures on a shortcut.
	for _, p := range []string{"/sh/a.txt", "/sh/b.txt"} {
		r := WorkerResult{Path: p, HTTPStatus: 507, ShortcutKey: shortcutKey}
		ss.UpdateScope(&r)
		advance(1 * time.Second)
	}

	// Success on same shortcut resets its window.
	ss.RecordSuccess(&WorkerResult{Path: "/sh/ok.txt", ShortcutKey: shortcutKey})

	// Need three fresh unique paths to trigger again.
	for _, p := range []string{"/sh/c.txt", "/sh/d.txt"} {
		r := WorkerResult{Path: p, HTTPStatus: 507, ShortcutKey: shortcutKey}
		result := ss.UpdateScope(&r)
		assert.False(t, result.Block)
		advance(1 * time.Second)
	}

	r := WorkerResult{Path: "/sh/e.txt", HTTPStatus: 507, ShortcutKey: shortcutKey}
	result := ss.UpdateScope(&r)
	require.True(t, result.Block)
	assert.Equal(t, "quota:shortcut:"+shortcutKey, result.ScopeKey)
}

// TestScope_SameFileDoesNotEscalate verifies that repeated failures from
// the same file path do not trigger a scope block, since the sliding window
// counts unique paths.
func TestScope_SameFileDoesNotEscalate(t *testing.T) {
	t.Parallel()

	clock, advance := controllableClock()
	ss := NewScopeState(clock, discardLogger())

	// Feed 10 failures on the same path — should never trigger.
	for i := 0; i < 10; i++ {
		r := WorkerResult{
			Path:       "/same-file.txt",
			HTTPStatus: 507,
		}
		result := ss.UpdateScope(&r)
		assert.False(t, result.Block, "repeated failures on the same path must not trigger scope block (iteration %d)", i)
		advance(500 * time.Millisecond)
	}
}

// TestScope_UpdateScopeOutagePattern verifies that 400 outage patterns
// feed the service sliding window and can trigger a service block.
func TestScope_UpdateScopeOutagePattern(t *testing.T) {
	t.Parallel()

	clock, advance := controllableClock()
	ss := NewScopeState(clock, discardLogger())

	// Five unique paths via outage pattern should trigger a service block.
	paths := []string{"/a.txt", "/b.txt", "/c.txt", "/d.txt", "/e.txt"}
	for i, p := range paths {
		result := ss.UpdateScopeOutagePattern(p)
		if i < len(paths)-1 {
			assert.False(t, result.Block, "outage pattern path %d should not trigger block yet", i)
		} else {
			require.True(t, result.Block, "fifth unique outage-pattern path must trigger service block")
			assert.Equal(t, "service", result.ScopeKey)
			assert.Equal(t, "service_outage", result.IssueType)
			assert.Equal(t, serviceInitialInterval, result.TrialInterval)
		}
		advance(2 * time.Second)
	}
}

// TestScope_OutagePatternSharesWindowWith5xx verifies that 400 outage
// patterns and 5xx errors feed the same service sliding window.
func TestScope_OutagePatternSharesWindowWith5xx(t *testing.T) {
	t.Parallel()

	clock, advance := controllableClock()
	ss := NewScopeState(clock, discardLogger())

	// Three 5xx failures.
	for _, p := range []string{"/a.txt", "/b.txt", "/c.txt"} {
		r := WorkerResult{Path: p, HTTPStatus: 502}
		result := ss.UpdateScope(&r)
		assert.False(t, result.Block)
		advance(1 * time.Second)
	}

	// One outage pattern.
	result := ss.UpdateScopeOutagePattern("/d.txt")
	assert.False(t, result.Block)
	advance(1 * time.Second)

	// Fifth unique path via outage pattern — should trigger because
	// 5xx and outage patterns share the same "service" window.
	result = ss.UpdateScopeOutagePattern("/e.txt")
	require.True(t, result.Block, "outage patterns and 5xx share the service window")
	assert.Equal(t, "service", result.ScopeKey)
}

// TestScope_NonScopeStatusReturnsEmpty verifies that non-scope HTTP
// statuses (e.g. 404, 409) produce a zero-value ScopeUpdateResult.
func TestScope_NonScopeStatusReturnsEmpty(t *testing.T) {
	t.Parallel()

	clock, _ := controllableClock()
	ss := NewScopeState(clock, discardLogger())

	cases := []struct {
		name   string
		status int
	}{
		{"404", 404},
		{"409", 409},
		{"400", 400},
		{"401", 401},
		{"200", 200},
		{"0", 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := WorkerResult{Path: "/file.txt", HTTPStatus: tc.status}
			result := ss.UpdateScope(&r)
			assert.False(t, result.Block, "status %d should not trigger a scope block", tc.status)
			assert.Empty(t, result.ScopeKey)
			assert.Empty(t, result.IssueType)
		})
	}
}

// TestScope_SuccessResetsServiceWindow verifies that a success resets the
// service sliding window in addition to quota windows.
func TestScope_SuccessResetsServiceWindow(t *testing.T) {
	t.Parallel()

	clock, advance := controllableClock()
	ss := NewScopeState(clock, discardLogger())

	// Four 5xx failures — one short of triggering.
	for _, p := range []string{"/a.txt", "/b.txt", "/c.txt", "/d.txt"} {
		r := WorkerResult{Path: p, HTTPStatus: 500}
		result := ss.UpdateScope(&r)
		assert.False(t, result.Block)
		advance(1 * time.Second)
	}

	// Success resets the service window.
	ss.RecordSuccess(&WorkerResult{Path: "/ok.txt"})

	// Now we need five fresh unique paths to trigger again.
	// The next failure (5th overall but 1st after reset) must not trigger.
	r := WorkerResult{Path: "/e.txt", HTTPStatus: 500}
	result := ss.UpdateScope(&r)
	assert.False(t, result.Block, "first failure after service window reset should not trigger")
}
