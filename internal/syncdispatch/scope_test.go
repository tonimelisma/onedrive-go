package syncdispatch

import (
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// discardLogger returns a logger that writes to nowhere, suitable for tests.
func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// controllableClock returns a nowFunc and a function to advance the clock.
// The clock starts at a fixed epoch to keep tests deterministic.
func controllableClock() (nowFunc func() time.Time, advance func(d time.Duration)) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	return func() time.Time { return now },
		func(d time.Duration) { now = now.Add(d) }
}

// Validates: R-2.10.3, R-2.10.26
func TestScope_429FallbackInterval(t *testing.T) {
	t.Parallel()

	clock, _ := controllableClock()
	ss := NewScopeState(clock, discardLogger())

	// 429 without Retry-After should use the defaultInitialTrialInterval fallback.
	r := synctypes.WorkerResult{
		Path:       "/file-a.txt",
		HTTPStatus: 429,
	}
	result := ss.UpdateScope(&r)

	require.True(t, result.Block)
	assert.Equal(t, synctypes.SKThrottleAccount(), result.ScopeKey)
	assert.Zero(t, result.RetryAfter, "429 without Retry-After should have zero RetryAfter")
}

// Validates: R-2.10.3, R-2.10.26
func TestScope_ImmediateRetryAfterBlocks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		path       string
		status     int
		retryAfter time.Duration
		wantScope  synctypes.ScopeKey
		wantIssue  string
	}{
		{
			name:       "rate_limited",
			path:       "/file-a.txt",
			status:     429,
			retryAfter: 90 * time.Second,
			wantScope:  synctypes.SKThrottleAccount(),
			wantIssue:  "rate_limited",
		},
		{
			name:       "service_outage",
			path:       "/doc.docx",
			status:     503,
			retryAfter: 120 * time.Second,
			wantScope:  synctypes.SKService(),
			wantIssue:  "service_outage",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clock, _ := controllableClock()
			ss := NewScopeState(clock, discardLogger())

			result := ss.UpdateScope(&synctypes.WorkerResult{
				Path:       tt.path,
				HTTPStatus: tt.status,
				RetryAfter: tt.retryAfter,
			})

			require.True(t, result.Block, "Retry-After should force an immediate scope block")
			assert.Equal(t, tt.wantScope, result.ScopeKey)
			assert.Equal(t, tt.wantIssue, result.IssueType)
			assert.Equal(t, tt.retryAfter, result.RetryAfter, "RetryAfter should be passed through")
		})
	}
}

// Validates: R-2.10.3
func TestScope_503WithoutRetryAfter(t *testing.T) {
	t.Parallel()

	clock, _ := controllableClock()
	ss := NewScopeState(clock, discardLogger())

	// A 503 without Retry-After should feed the service sliding window,
	// not trigger an immediate block (it falls into the >= 500 case).
	r := synctypes.WorkerResult{
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
		r := synctypes.WorkerResult{
			Path:       p,
			HTTPStatus: 507,
		}
		result := ss.UpdateScope(&r)
		if i < len(paths)-1 {
			assert.False(t, result.Block, "path %d (%s) should not trigger block yet", i, p)
		} else {
			require.True(t, result.Block, "third unique path must trigger quota:own block")
			assert.Equal(t, synctypes.SKQuotaOwn(), result.ScopeKey)
			assert.Equal(t, "quota_exceeded", result.IssueType)
			assert.Zero(t, result.RetryAfter)
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
		r := synctypes.WorkerResult{
			Path:        p,
			HTTPStatus:  507,
			ShortcutKey: shortcutKey,
		}
		result := ss.UpdateScope(&r)
		if i < len(paths)-1 {
			assert.False(t, result.Block, "path %d should not trigger block yet", i)
		} else {
			require.True(t, result.Block, "third unique shortcut path must trigger quota:shortcut block")
			assert.Equal(t, synctypes.SKQuotaShortcut(shortcutKey), result.ScopeKey)
			assert.Equal(t, "quota_exceeded", result.IssueType)
			assert.Zero(t, result.RetryAfter)
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
		r := synctypes.WorkerResult{Path: p, HTTPStatus: 507, ShortcutKey: keyA}
		result := ss.UpdateScope(&r)
		assert.False(t, result.Block)
		advance(1 * time.Second)
	}

	// Feed three failures on shortcut B — should trigger block for B only.
	for i, p := range []string{"/shared-b/1.txt", "/shared-b/2.txt", "/shared-b/3.txt"} {
		r := synctypes.WorkerResult{Path: p, HTTPStatus: 507, ShortcutKey: keyB}
		result := ss.UpdateScope(&r)
		if i < 2 {
			assert.False(t, result.Block)
		} else {
			require.True(t, result.Block, "shortcut B must trigger independently")
			assert.Equal(t, synctypes.SKQuotaShortcut(keyB), result.ScopeKey)
		}
		advance(1 * time.Second)
	}

	// One more failure on shortcut A — still at 2 unique paths after B
	// triggered, so A must not block (only 3 unique paths needed but we
	// only have 2 for A).
	rA3 := synctypes.WorkerResult{Path: "/shared-a/3.txt", HTTPStatus: 507, ShortcutKey: keyA}
	resultA3 := ss.UpdateScope(&rA3)
	require.True(t, resultA3.Block, "third unique path on shortcut A should now trigger")
	assert.Equal(t, synctypes.SKQuotaShortcut(keyA), resultA3.ScopeKey,
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
		r := synctypes.WorkerResult{
			Path:       p,
			HTTPStatus: 500,
		}
		result := ss.UpdateScope(&r)
		if i < len(paths)-1 {
			assert.False(t, result.Block, "path %d should not trigger block yet", i)
		} else {
			require.True(t, result.Block, "fifth unique path must trigger service block")
			assert.Equal(t, synctypes.SKService(), result.ScopeKey)
			assert.Equal(t, "service_outage", result.IssueType)
			assert.Zero(t, result.RetryAfter)
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
		r := synctypes.WorkerResult{Path: p, HTTPStatus: 500}
		result := ss.UpdateScope(&r)
		assert.False(t, result.Block)
		advance(1 * time.Second)
	}

	// Advance past the 30s window so the first entries expire.
	advance(30 * time.Second)

	r := synctypes.WorkerResult{Path: "/e.txt", HTTPStatus: 500}
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
		r := synctypes.WorkerResult{Path: p, HTTPStatus: 507}
		result := ss.UpdateScope(&r)
		assert.False(t, result.Block)
		advance(1 * time.Second)
	}

	// Record a success — this must reset the quota:own window.
	ss.RecordSuccess(&synctypes.WorkerResult{Path: "/ok.txt"})

	// Now three more unique failures are needed to trigger the block.
	// The next failure (3rd unique path overall but 1st after reset)
	// must NOT trigger.
	for i, p := range []string{"/c.txt", "/d.txt"} {
		r := synctypes.WorkerResult{Path: p, HTTPStatus: 507}
		result := ss.UpdateScope(&r)
		assert.False(t, result.Block, "after reset, path %d should not trigger", i)
		advance(1 * time.Second)
	}

	// Third unique path after reset should trigger.
	r := synctypes.WorkerResult{Path: "/e.txt", HTTPStatus: 507}
	result := ss.UpdateScope(&r)
	require.True(t, result.Block, "third unique path after reset should trigger quota:own")
	assert.Equal(t, synctypes.SKQuotaOwn(), result.ScopeKey)
}

// Validates: R-2.10.42
func TestScope_SuccessResetsShortcutWindow(t *testing.T) {
	t.Parallel()

	clock, advance := controllableClock()
	ss := NewScopeState(clock, discardLogger())

	shortcutKey := "driveX:itemX"

	// Two failures on a shortcut.
	for _, p := range []string{"/sh/a.txt", "/sh/b.txt"} {
		r := synctypes.WorkerResult{Path: p, HTTPStatus: 507, ShortcutKey: shortcutKey}
		ss.UpdateScope(&r)
		advance(1 * time.Second)
	}

	// Success on same shortcut resets its window.
	ss.RecordSuccess(&synctypes.WorkerResult{Path: "/sh/ok.txt", ShortcutKey: shortcutKey})

	// Need three fresh unique paths to trigger again.
	for _, p := range []string{"/sh/c.txt", "/sh/d.txt"} {
		r := synctypes.WorkerResult{Path: p, HTTPStatus: 507, ShortcutKey: shortcutKey}
		result := ss.UpdateScope(&r)
		assert.False(t, result.Block)
		advance(1 * time.Second)
	}

	r := synctypes.WorkerResult{Path: "/sh/e.txt", HTTPStatus: 507, ShortcutKey: shortcutKey}
	result := ss.UpdateScope(&r)
	require.True(t, result.Block)
	assert.Equal(t, synctypes.SKQuotaShortcut(shortcutKey), result.ScopeKey)
}

// Validates: R-2.10.3
// TestScope_SameFileDoesNotEscalate verifies that repeated failures from
// the same file path do not trigger a scope block, since the sliding window
// counts unique paths.
func TestScope_SameFileDoesNotEscalate(t *testing.T) {
	t.Parallel()

	clock, advance := controllableClock()
	ss := NewScopeState(clock, discardLogger())

	// Feed 10 failures on the same path — should never trigger.
	for i := range 10 {
		r := synctypes.WorkerResult{
			Path:       "/same-file.txt",
			HTTPStatus: 507,
		}
		result := ss.UpdateScope(&r)
		assert.False(t, result.Block, "repeated failures on the same path must not trigger scope block (iteration %d)", i)
		advance(500 * time.Millisecond)
	}
}

// Validates: R-2.10.3, R-2.10.28
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
			assert.Equal(t, synctypes.SKService(), result.ScopeKey)
			assert.Equal(t, "service_outage", result.IssueType)
			assert.Zero(t, result.RetryAfter)
		}
		advance(2 * time.Second)
	}
}

// Validates: R-2.10.3, R-2.10.28
// TestScope_OutagePatternSharesWindowWith5xx verifies that 400 outage
// patterns and 5xx errors feed the same service sliding window.
func TestScope_OutagePatternSharesWindowWith5xx(t *testing.T) {
	t.Parallel()

	clock, advance := controllableClock()
	ss := NewScopeState(clock, discardLogger())

	// Three 5xx failures.
	for _, p := range []string{"/a.txt", "/b.txt", "/c.txt"} {
		r := synctypes.WorkerResult{Path: p, HTTPStatus: 502}
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
	assert.Equal(t, synctypes.SKService(), result.ScopeKey)
}

// Validates: R-2.10.3
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
			r := synctypes.WorkerResult{Path: "/file.txt", HTTPStatus: tc.status}
			result := ss.UpdateScope(&r)
			assert.False(t, result.Block, "status %d should not trigger a scope block", tc.status)
			assert.Empty(t, result.ScopeKey)
			assert.Empty(t, result.IssueType)
		})
	}
}

// Validates: R-2.10.42
// TestScope_SuccessResetsServiceWindow verifies that a success resets the
// service sliding window in addition to quota windows.
func TestScope_SuccessResetsServiceWindow(t *testing.T) {
	t.Parallel()

	clock, advance := controllableClock()
	ss := NewScopeState(clock, discardLogger())

	// Four 5xx failures — one short of triggering.
	for _, p := range []string{"/a.txt", "/b.txt", "/c.txt", "/d.txt"} {
		r := synctypes.WorkerResult{Path: p, HTTPStatus: 500}
		result := ss.UpdateScope(&r)
		assert.False(t, result.Block)
		advance(1 * time.Second)
	}

	// Success resets the service window.
	ss.RecordSuccess(&synctypes.WorkerResult{Path: "/ok.txt"})

	// Now we need five fresh unique paths to trigger again.
	// The next failure (5th overall but 1st after reset) must not trigger.
	r := synctypes.WorkerResult{Path: "/e.txt", HTTPStatus: 500}
	result := ss.UpdateScope(&r)
	assert.False(t, result.Block, "first failure after service window reset should not trigger")
}

// ---------------------------------------------------------------------------
// synctypes.ScopeKey type system tests
// ---------------------------------------------------------------------------

// Validates: R-2.10
func TestScopeKey_StringRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		key  synctypes.ScopeKey
		wire string
	}{
		{"throttle:account", synctypes.SKThrottleAccount(), "throttle:account"},
		{"service", synctypes.SKService(), "service"},
		{"quota:own", synctypes.SKQuotaOwn(), "quota:own"},
		{"disk:local", synctypes.SKDiskLocal(), "disk:local"},
		{"quota:shortcut", synctypes.SKQuotaShortcut("driveA:itemB"), "quota:shortcut:driveA:itemB"},
		{"perm:dir", synctypes.SKPermDir("Documents/Private"), "perm:dir:Documents/Private"},
		{"perm:remote", synctypes.SKPermRemote("Shared/TeamDocs"), "perm:remote:Shared/TeamDocs"},
	}

	for _, tt := range tests {
		// String() produces the expected wire format.
		assert.Equal(t, tt.wire, tt.key.String(), "%s String()", tt.name)

		// synctypes.ParseScopeKey round-trips back to the original key.
		parsed := synctypes.ParseScopeKey(tt.wire)
		assert.Equal(t, tt.key, parsed, "%s synctypes.ParseScopeKey round-trip", tt.name)
	}
}

// Validates: R-2.10
func TestParseScopeKey_Unknown(t *testing.T) {
	t.Parallel()

	// Unknown wire format produces zero-value synctypes.ScopeKey.
	sk := synctypes.ParseScopeKey("unknown:format")
	assert.True(t, sk.IsZero(), "unknown format should produce zero synctypes.ScopeKey")

	sk = synctypes.ParseScopeKey("")
	assert.True(t, sk.IsZero(), "empty string should produce zero synctypes.ScopeKey")
}

// Validates: R-2.10
func TestScopeKey_IsZero(t *testing.T) {
	t.Parallel()

	assert.True(t, synctypes.ScopeKey{}.IsZero())
	assert.False(t, synctypes.SKThrottleAccount().IsZero())
	assert.False(t, synctypes.SKPermDir("x").IsZero())
}

// Validates: R-2.10
func TestScopeKey_IsGlobal(t *testing.T) {
	t.Parallel()

	assert.True(t, synctypes.SKThrottleAccount().IsGlobal())
	assert.True(t, synctypes.SKService().IsGlobal())
	assert.False(t, synctypes.SKQuotaOwn().IsGlobal())
	assert.False(t, synctypes.SKQuotaShortcut("a:b").IsGlobal())
	assert.False(t, synctypes.SKPermDir("x").IsGlobal())
	assert.False(t, synctypes.SKPermRemote("x").IsGlobal())
	assert.False(t, synctypes.SKDiskLocal().IsGlobal())
}

// Validates: R-2.10
func TestScopeKey_IsPermDir(t *testing.T) {
	t.Parallel()

	assert.True(t, synctypes.SKPermDir("Documents").IsPermDir())
	assert.False(t, synctypes.SKThrottleAccount().IsPermDir())
	assert.False(t, synctypes.SKQuotaOwn().IsPermDir())
}

// Validates: R-2.10.34
func TestScopeKey_IsPermRemote(t *testing.T) {
	t.Parallel()

	assert.True(t, synctypes.SKPermRemote("Shared/TeamDocs").IsPermRemote())
	assert.False(t, synctypes.SKPermDir("Documents").IsPermRemote())
	assert.False(t, synctypes.SKThrottleAccount().IsPermRemote())
}

// Validates: R-2.10
func TestScopeKey_DirPath(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "Documents/Private", synctypes.SKPermDir("Documents/Private").DirPath())

	// DirPath on non-PermDir should panic.
	assert.Panics(t, func() { synctypes.SKThrottleAccount().DirPath() })
}

// Validates: R-2.10.34
func TestScopeKey_RemotePath(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "Shared/TeamDocs", synctypes.SKPermRemote("Shared/TeamDocs").RemotePath())
	assert.Panics(t, func() { synctypes.SKThrottleAccount().RemotePath() })
}

// Validates: R-2.10
func TestScopeKey_IssueType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		key  synctypes.ScopeKey
		want string
	}{
		{synctypes.SKThrottleAccount(), synctypes.IssueRateLimited},
		{synctypes.SKService(), synctypes.IssueServiceOutage},
		{synctypes.SKQuotaOwn(), synctypes.IssueQuotaExceeded},
		{synctypes.SKQuotaShortcut("a:b"), synctypes.IssueQuotaExceeded},
		{synctypes.SKPermDir("x"), synctypes.IssueLocalPermissionDenied},
		{synctypes.SKPermRemote("Shared/TeamDocs"), synctypes.IssuePermissionDenied},
		{synctypes.SKDiskLocal(), synctypes.IssueDiskFull},
		{synctypes.ScopeKey{}, ""}, // zero value
	}

	for _, tt := range tests {
		assert.Equal(t, tt.want, tt.key.IssueType(), "IssueType for %s", tt.key)
	}
}

// Validates: R-2.10
func TestScopeKey_Humanize(t *testing.T) {
	t.Parallel()

	shortcuts := []synctypes.Shortcut{
		{RemoteDrive: "driveA", RemoteItem: "itemB", LocalPath: "/mnt/shared/TeamDocs"},
	}

	assert.Equal(t, "your OneDrive account (rate limited)", synctypes.SKThrottleAccount().Humanize(nil))
	assert.Equal(t, "OneDrive service", synctypes.SKService().Humanize(nil))
	assert.Equal(t, "your OneDrive storage", synctypes.SKQuotaOwn().Humanize(nil))
	assert.Equal(t, "local disk", synctypes.SKDiskLocal().Humanize(nil))
	assert.Equal(t, "Documents/Private", synctypes.SKPermDir("Documents/Private").Humanize(nil))
	assert.Equal(t, "Shared/TeamDocs", synctypes.SKPermRemote("Shared/TeamDocs").Humanize(nil))

	// synctypes.Shortcut found by local path.
	assert.Equal(t, "/mnt/shared/TeamDocs", synctypes.SKQuotaShortcut("driveA:itemB").Humanize(shortcuts))

	// synctypes.Shortcut not found — falls back to composite key.
	assert.Equal(t, "driveX:itemY", synctypes.SKQuotaShortcut("driveX:itemY").Humanize(nil))
}

// Validates: R-2.10
func TestScopeKey_BlocksAction(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		key             synctypes.ScopeKey
		path            string
		shortcutKey     string
		actionType      synctypes.ActionType
		targetsOwnDrive bool
		want            bool
	}{
		// Global scopes block everything.
		{"throttle blocks upload", synctypes.SKThrottleAccount(), "/a.txt", "", synctypes.ActionUpload, true, true},
		{"throttle blocks download", synctypes.SKThrottleAccount(), "/a.txt", "", synctypes.ActionDownload, true, true},
		{"service blocks all", synctypes.SKService(), "/a.txt", "sc:1", synctypes.ActionUpload, false, true},

		// Disk:local blocks downloads only.
		{"disk blocks download", synctypes.SKDiskLocal(), "/a.txt", "", synctypes.ActionDownload, true, true},
		{"disk passes upload", synctypes.SKDiskLocal(), "/a.txt", "", synctypes.ActionUpload, true, false},

		// Quota:own blocks own-drive uploads.
		{"quota own blocks own upload", synctypes.SKQuotaOwn(), "/a.txt", "", synctypes.ActionUpload, true, true},
		{"quota own passes download", synctypes.SKQuotaOwn(), "/a.txt", "", synctypes.ActionDownload, true, false},
		{"quota own passes shortcut upload", synctypes.SKQuotaOwn(), "/a.txt", "sc:1", synctypes.ActionUpload, false, false},

		// Quota:shortcut blocks matching shortcut uploads.
		{"shortcut blocks matching upload", synctypes.SKQuotaShortcut("sc:1"), "/a.txt", "sc:1", synctypes.ActionUpload, false, true},
		{"shortcut passes wrong shortcut", synctypes.SKQuotaShortcut("sc:1"), "/a.txt", "sc:2", synctypes.ActionUpload, false, false},
		{"shortcut passes download", synctypes.SKQuotaShortcut("sc:1"), "/a.txt", "sc:1", synctypes.ActionDownload, false, false},

		// Perm:dir blocks paths under the directory.
		{"perm blocks exact dir", synctypes.SKPermDir("Private"), "Private", "", synctypes.ActionUpload, true, true},
		{"perm blocks subpath", synctypes.SKPermDir("Private"), "Private/secret.txt", "", synctypes.ActionUpload, true, true},
		{"perm passes outside", synctypes.SKPermDir("Private"), "Public/readme.txt", "", synctypes.ActionUpload, true, false},

		// Perm:remote blocks remote mutations recursively, but still allows downloads/local-only work.
		{"perm remote blocks upload", synctypes.SKPermRemote("Shared/TeamDocs"), "Shared/TeamDocs/file.txt", "", synctypes.ActionUpload, false, true},
		{"perm remote blocks nested remote delete", synctypes.SKPermRemote("Shared/TeamDocs"), "Shared/TeamDocs/nested/file.txt", "", synctypes.ActionRemoteDelete, false, true},
		{"perm remote blocks folder create", synctypes.SKPermRemote("Shared/TeamDocs"), "Shared/TeamDocs/newdir", "", synctypes.ActionFolderCreate, false, true},
		{"perm remote passes download", synctypes.SKPermRemote("Shared/TeamDocs"), "Shared/TeamDocs/file.txt", "", synctypes.ActionDownload, false, false},
		{"perm remote passes local delete", synctypes.SKPermRemote("Shared/TeamDocs"), "Shared/TeamDocs/file.txt", "", synctypes.ActionLocalDelete, false, false},
		{"perm remote passes outside", synctypes.SKPermRemote("Shared/TeamDocs"), "Shared/Other/file.txt", "", synctypes.ActionUpload, false, false},
	}

	for _, tt := range tests {
		got := tt.key.BlocksAction(tt.path, tt.shortcutKey, tt.actionType, tt.targetsOwnDrive)
		assert.Equal(t, tt.want, got, tt.name)
	}
}

// Validates: R-2.10.1, R-2.10.3
func TestScopeKeyForStatus(t *testing.T) {
	t.Parallel()

	assert.Equal(t, synctypes.SKThrottleAccount(), synctypes.ScopeKeyForStatus(429, ""))
	assert.Equal(t, synctypes.SKThrottleAccount(), synctypes.ScopeKeyForStatus(429, "sc:1")) // 429 is always account-level
	assert.Equal(t, synctypes.SKService(), synctypes.ScopeKeyForStatus(503, ""))
	assert.Equal(t, synctypes.SKService(), synctypes.ScopeKeyForStatus(500, ""))
	assert.Equal(t, synctypes.SKService(), synctypes.ScopeKeyForStatus(502, ""))
	assert.Equal(t, synctypes.SKQuotaOwn(), synctypes.ScopeKeyForStatus(507, ""))
	assert.Equal(t, synctypes.SKQuotaShortcut("drive1:item1"), synctypes.ScopeKeyForStatus(507, "drive1:item1"))
	assert.True(t, synctypes.ScopeKeyForStatus(404, "").IsZero(), "non-scope status should be zero")
	assert.True(t, synctypes.ScopeKeyForStatus(200, "").IsZero(), "success status should be zero")
}
