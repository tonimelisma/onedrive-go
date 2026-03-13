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
	assert.Equal(t, SKThrottleAccount, result.ScopeKey)
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
	assert.Equal(t, SKThrottleAccount, result.ScopeKey)
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
	assert.Equal(t, SKService, result.ScopeKey)
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
			assert.Equal(t, SKQuotaOwn, result.ScopeKey)
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
			assert.Equal(t, SKQuotaShortcut(shortcutKey), result.ScopeKey)
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
			assert.Equal(t, SKQuotaShortcut(keyB), result.ScopeKey)
		}
		advance(1 * time.Second)
	}

	// One more failure on shortcut A — still at 2 unique paths after B
	// triggered, so A must not block (only 3 unique paths needed but we
	// only have 2 for A).
	rA3 := WorkerResult{Path: "/shared-a/3.txt", HTTPStatus: 507, ShortcutKey: keyA}
	resultA3 := ss.UpdateScope(&rA3)
	require.True(t, resultA3.Block, "third unique path on shortcut A should now trigger")
	assert.Equal(t, SKQuotaShortcut(keyA), resultA3.ScopeKey,
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
			assert.Equal(t, SKService, result.ScopeKey)
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
	assert.Equal(t, SKQuotaOwn, result.ScopeKey)
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
	assert.Equal(t, SKQuotaShortcut(shortcutKey), result.ScopeKey)
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
			assert.Equal(t, SKService, result.ScopeKey)
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
	assert.Equal(t, SKService, result.ScopeKey)
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

// ---------------------------------------------------------------------------
// ScopeKey.MaxTrialInterval (R-2.10.14)
// ---------------------------------------------------------------------------

// Validates: R-2.10.6, R-2.10.8, R-2.10.14
func TestScopeKey_MaxTrialInterval(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		key  ScopeKey
		want time.Duration
	}{
		{"quota:own", SKQuotaOwn, 1 * time.Hour},
		{"quota:shortcut", SKQuotaShortcut("driveX:itemY"), 1 * time.Hour},
		// Validates: R-2.10.43
		{"disk:local", SKDiskLocal, 1 * time.Hour}, // same cap as quota — disk free changes slowly
		{"throttle:account", SKThrottleAccount, 10 * time.Minute},
		{"service", SKService, 10 * time.Minute},
		{"zero-value (unknown)", ScopeKey{}, 10 * time.Minute}, // default
	}

	for _, tt := range tests {
		assert.Equal(t, tt.want, tt.key.MaxTrialInterval(),
			"ScopeKey(%s).MaxTrialInterval()", tt.name)
	}
}

// ---------------------------------------------------------------------------
// ScopeKey type system tests
// ---------------------------------------------------------------------------

func TestScopeKey_StringRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		key  ScopeKey
		wire string
	}{
		{"throttle:account", SKThrottleAccount, "throttle:account"},
		{"service", SKService, "service"},
		{"quota:own", SKQuotaOwn, "quota:own"},
		{"disk:local", SKDiskLocal, "disk:local"},
		{"quota:shortcut", SKQuotaShortcut("driveA:itemB"), "quota:shortcut:driveA:itemB"},
		{"perm:dir", SKPermDir("Documents/Private"), "perm:dir:Documents/Private"},
	}

	for _, tt := range tests {
		// String() produces the expected wire format.
		assert.Equal(t, tt.wire, tt.key.String(), "%s String()", tt.name)

		// ParseScopeKey round-trips back to the original key.
		parsed := ParseScopeKey(tt.wire)
		assert.Equal(t, tt.key, parsed, "%s ParseScopeKey round-trip", tt.name)
	}
}

func TestParseScopeKey_Unknown(t *testing.T) {
	t.Parallel()

	// Unknown wire format produces zero-value ScopeKey.
	sk := ParseScopeKey("unknown:format")
	assert.True(t, sk.IsZero(), "unknown format should produce zero ScopeKey")

	sk = ParseScopeKey("")
	assert.True(t, sk.IsZero(), "empty string should produce zero ScopeKey")
}

func TestScopeKey_IsZero(t *testing.T) {
	t.Parallel()

	assert.True(t, ScopeKey{}.IsZero())
	assert.False(t, SKThrottleAccount.IsZero())
	assert.False(t, SKPermDir("x").IsZero())
}

func TestScopeKey_IsGlobal(t *testing.T) {
	t.Parallel()

	assert.True(t, SKThrottleAccount.IsGlobal())
	assert.True(t, SKService.IsGlobal())
	assert.False(t, SKQuotaOwn.IsGlobal())
	assert.False(t, SKQuotaShortcut("a:b").IsGlobal())
	assert.False(t, SKPermDir("x").IsGlobal())
	assert.False(t, SKDiskLocal.IsGlobal())
}

func TestScopeKey_IsPermDir(t *testing.T) {
	t.Parallel()

	assert.True(t, SKPermDir("Documents").IsPermDir())
	assert.False(t, SKThrottleAccount.IsPermDir())
	assert.False(t, SKQuotaOwn.IsPermDir())
}

func TestScopeKey_DirPath(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "Documents/Private", SKPermDir("Documents/Private").DirPath())

	// DirPath on non-PermDir should panic.
	assert.Panics(t, func() { SKThrottleAccount.DirPath() })
}

func TestScopeKey_IssueType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		key  ScopeKey
		want string
	}{
		{SKThrottleAccount, IssueRateLimited},
		{SKService, IssueServiceOutage},
		{SKQuotaOwn, IssueQuotaExceeded},
		{SKQuotaShortcut("a:b"), IssueQuotaExceeded},
		{SKPermDir("x"), IssueLocalPermissionDenied},
		{SKDiskLocal, IssueDiskFull},
		{ScopeKey{}, ""}, // zero value
	}

	for _, tt := range tests {
		assert.Equal(t, tt.want, tt.key.IssueType(), "IssueType for %s", tt.key)
	}
}

func TestScopeKey_Humanize(t *testing.T) {
	t.Parallel()

	shortcuts := []Shortcut{
		{RemoteDrive: "driveA", RemoteItem: "itemB", LocalPath: "/mnt/shared/TeamDocs"},
	}

	assert.Equal(t, "your OneDrive account (rate limited)", SKThrottleAccount.Humanize(nil))
	assert.Equal(t, "OneDrive service", SKService.Humanize(nil))
	assert.Equal(t, "your OneDrive storage", SKQuotaOwn.Humanize(nil))
	assert.Equal(t, "local disk", SKDiskLocal.Humanize(nil))
	assert.Equal(t, "Documents/Private", SKPermDir("Documents/Private").Humanize(nil))

	// Shortcut found by local path.
	assert.Equal(t, "/mnt/shared/TeamDocs", SKQuotaShortcut("driveA:itemB").Humanize(shortcuts))

	// Shortcut not found — falls back to composite key.
	assert.Equal(t, "driveX:itemY", SKQuotaShortcut("driveX:itemY").Humanize(nil))
}

func TestScopeKey_BlocksAction(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		key             ScopeKey
		path            string
		shortcutKey     string
		actionType      ActionType
		targetsOwnDrive bool
		want            bool
	}{
		// Global scopes block everything.
		{"throttle blocks upload", SKThrottleAccount, "/a.txt", "", ActionUpload, true, true},
		{"throttle blocks download", SKThrottleAccount, "/a.txt", "", ActionDownload, true, true},
		{"service blocks all", SKService, "/a.txt", "sc:1", ActionUpload, false, true},

		// Disk:local blocks downloads only.
		{"disk blocks download", SKDiskLocal, "/a.txt", "", ActionDownload, true, true},
		{"disk passes upload", SKDiskLocal, "/a.txt", "", ActionUpload, true, false},

		// Quota:own blocks own-drive uploads.
		{"quota own blocks own upload", SKQuotaOwn, "/a.txt", "", ActionUpload, true, true},
		{"quota own passes download", SKQuotaOwn, "/a.txt", "", ActionDownload, true, false},
		{"quota own passes shortcut upload", SKQuotaOwn, "/a.txt", "sc:1", ActionUpload, false, false},

		// Quota:shortcut blocks matching shortcut uploads.
		{"shortcut blocks matching upload", SKQuotaShortcut("sc:1"), "/a.txt", "sc:1", ActionUpload, false, true},
		{"shortcut passes wrong shortcut", SKQuotaShortcut("sc:1"), "/a.txt", "sc:2", ActionUpload, false, false},
		{"shortcut passes download", SKQuotaShortcut("sc:1"), "/a.txt", "sc:1", ActionDownload, false, false},

		// Perm:dir blocks paths under the directory.
		{"perm blocks exact dir", SKPermDir("Private"), "Private", "", ActionUpload, true, true},
		{"perm blocks subpath", SKPermDir("Private"), "Private/secret.txt", "", ActionUpload, true, true},
		{"perm passes outside", SKPermDir("Private"), "Public/readme.txt", "", ActionUpload, true, false},
	}

	for _, tt := range tests {
		got := tt.key.BlocksAction(tt.path, tt.shortcutKey, tt.actionType, tt.targetsOwnDrive)
		assert.Equal(t, tt.want, got, tt.name)
	}
}

// Validates: R-2.10.1, R-2.10.3
func TestScopeKeyForStatus(t *testing.T) {
	t.Parallel()

	assert.Equal(t, SKThrottleAccount, ScopeKeyForStatus(429, ""))
	assert.Equal(t, SKThrottleAccount, ScopeKeyForStatus(429, "sc:1")) // 429 is always account-level
	assert.Equal(t, SKService, ScopeKeyForStatus(503, ""))
	assert.Equal(t, SKService, ScopeKeyForStatus(500, ""))
	assert.Equal(t, SKService, ScopeKeyForStatus(502, ""))
	assert.Equal(t, SKQuotaOwn, ScopeKeyForStatus(507, ""))
	assert.Equal(t, SKQuotaShortcut("drive1:item1"), ScopeKeyForStatus(507, "drive1:item1"))
	assert.True(t, ScopeKeyForStatus(404, "").IsZero(), "non-scope status should be zero")
	assert.True(t, ScopeKeyForStatus(200, "").IsZero(), "success status should be zero")
}
