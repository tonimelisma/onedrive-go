package sync

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Validates: R-2.10.3, R-2.10.26
func TestScope_429FallbackInterval(t *testing.T) {
	t.Parallel()

	clock, _ := controllableClock()
	ss := NewScopeState(clock, discardLogger())

	// 429 without Retry-After should use the defaultInitialTrialInterval fallback.
	r := WorkerResult{
		Path:          "/file-a.txt",
		HTTPStatus:    429,
		TargetDriveID: driveid.New("0000000000000001"),
	}
	result := ss.UpdateScope(&r)

	require.True(t, result.Block)
	assert.Equal(t, SKThrottleDrive(driveid.New("0000000000000001")), result.ScopeKey)
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
		wantScope  ScopeKey
		wantIssue  string
	}{
		{
			name:       "rate_limited",
			path:       "/file-a.txt",
			status:     429,
			retryAfter: 90 * time.Second,
			wantScope:  SKThrottleDrive(driveid.New("0000000000000001")),
			wantIssue:  "rate_limited",
		},
		{
			name:       "service_outage",
			path:       "/doc.docx",
			status:     503,
			retryAfter: 120 * time.Second,
			wantScope:  SKService(),
			wantIssue:  "service_outage",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clock, _ := controllableClock()
			ss := NewScopeState(clock, discardLogger())

			result := ss.UpdateScope(&WorkerResult{
				Path:          tt.path,
				HTTPStatus:    tt.status,
				RetryAfter:    tt.retryAfter,
				TargetDriveID: driveid.New("0000000000000001"),
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
			assert.Equal(t, SKQuotaOwn(), result.ScopeKey)
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
			assert.Equal(t, SKService(), result.ScopeKey)
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
	assert.Equal(t, SKQuotaOwn(), result.ScopeKey)
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
		r := WorkerResult{
			Path:       "/same-file.txt",
			HTTPStatus: 507,
		}
		result := ss.UpdateScope(&r)
		assert.False(t, result.Block, "repeated failures on the same path must not trigger scope block (iteration %d)", i)
		advance(500 * time.Millisecond)
	}
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
			r := WorkerResult{Path: "/file.txt", HTTPStatus: tc.status}
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
// ScopeKey type system tests
// ---------------------------------------------------------------------------

// Validates: R-2.10
func TestScopeKey_StringRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		key  ScopeKey
		wire string
	}{
		{"throttle:account", SKThrottleAccount(), "throttle:account"},
		{"throttle:target:drive", SKThrottleDrive(driveid.New("0000000000000001")), "throttle:target:drive:0000000000000001"},
		{"throttle:target:shared", SKThrottleShared("driveA", "itemB"), "throttle:target:shared:driveA:itemB"},
		{"service", SKService(), "service"},
		{"quota:own", SKQuotaOwn(), "quota:own"},
		{"disk:local", SKDiskLocal(), "disk:local"},
		{"quota:shortcut", SKQuotaShortcut("driveA:itemB"), "quota:shortcut:driveA:itemB"},
		{"perm:local-write", SKPermLocalWrite("Documents/Private"), "perm:local-write:Documents/Private"},
		{"perm:remote-write", SKPermRemoteWrite("Shared/TeamDocs"), "perm:remote-write:Shared/TeamDocs"},
	}

	for _, tt := range tests {
		// String() produces the expected wire format.
		assert.Equal(t, tt.wire, tt.key.String(), "%s String()", tt.name)

		// ParseScopeKey round-trips back to the original key.
		parsed := ParseScopeKey(tt.wire)
		assert.Equal(t, tt.key, parsed, "%s ParseScopeKey round-trip", tt.name)
	}
}

// Validates: R-2.10
func TestParseScopeKey_Unknown(t *testing.T) {
	t.Parallel()

	// Unknown wire format produces zero-value ScopeKey.
	sk := ParseScopeKey("unknown:format")
	assert.True(t, sk.IsZero(), "unknown format should produce zero ScopeKey")

	sk = ParseScopeKey("")
	assert.True(t, sk.IsZero(), "empty string should produce zero ScopeKey")
}

// Validates: R-2.10
func TestScopeKey_IsZero(t *testing.T) {
	t.Parallel()

	assert.True(t, ScopeKey{}.IsZero())
	assert.False(t, SKThrottleAccount().IsZero())
	assert.False(t, SKThrottleDrive(driveid.New("0000000000000001")).IsZero())
	assert.False(t, SKPermLocalWrite("x").IsZero())
}

// Validates: R-2.10
func TestScopeKey_IsGlobal(t *testing.T) {
	t.Parallel()

	assert.True(t, SKThrottleAccount().IsGlobal())
	assert.False(t, SKThrottleDrive(driveid.New("0000000000000001")).IsGlobal())
	assert.True(t, SKService().IsGlobal())
	assert.False(t, SKQuotaOwn().IsGlobal())
	assert.False(t, SKQuotaShortcut("a:b").IsGlobal())
	assert.False(t, SKPermLocalWrite("x").IsGlobal())
	assert.False(t, SKPermRemoteWrite("x").IsGlobal())
	assert.False(t, SKDiskLocal().IsGlobal())
}

// Validates: R-2.10
func TestScopeKey_IsPermLocalWrite(t *testing.T) {
	t.Parallel()

	assert.True(t, SKPermLocalWrite("Documents").IsPermLocalWrite())
	assert.False(t, SKThrottleAccount().IsPermLocalWrite())
	assert.False(t, SKQuotaOwn().IsPermLocalWrite())
}

// Validates: R-2.10.34
func TestScopeKey_IsPermRemoteWrite(t *testing.T) {
	t.Parallel()

	assert.True(t, SKPermRemoteWrite("Shared/TeamDocs").IsPermRemoteWrite())
	assert.False(t, SKPermLocalWrite("Documents").IsPermRemoteWrite())
	assert.False(t, SKThrottleAccount().IsPermRemoteWrite())
}

// Validates: R-2.10
func TestScopeKey_DirPath(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "Documents/Private", SKPermLocalWrite("Documents/Private").DirPath())

	// DirPath on non-PermDir should panic.
	assert.Panics(t, func() { SKThrottleAccount().DirPath() })
}

// Validates: R-2.10.34
func TestScopeKey_RemotePath(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "Shared/TeamDocs", SKPermRemoteWrite("Shared/TeamDocs").RemotePath())
	assert.Panics(t, func() { SKThrottleAccount().RemotePath() })
}

// Validates: R-2.10
func TestScopeKey_IssueType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		key  ScopeKey
		want string
	}{
		{SKThrottleAccount(), IssueRateLimited},
		{SKThrottleDrive(driveid.New("0000000000000001")), IssueRateLimited},
		{SKThrottleShared("driveA", "itemB"), IssueRateLimited},
		{SKService(), IssueServiceOutage},
		{SKQuotaOwn(), IssueQuotaExceeded},
		{SKQuotaShortcut("a:b"), IssueQuotaExceeded},
		{SKPermLocalWrite("x"), IssueLocalWriteDenied},
		{SKPermRemoteWrite("Shared/TeamDocs"), IssueRemoteWriteDenied},
		{SKDiskLocal(), IssueDiskFull},
		{ScopeKey{}, ""}, // zero value
	}

	for _, tt := range tests {
		assert.Equal(t, tt.want, tt.key.IssueType(), "IssueType for %s", tt.key)
	}
}

// Validates: R-2.10
func TestScopeKey_Humanize(t *testing.T) {
	t.Parallel()

	shortcuts := []Shortcut{
		{RemoteDrive: "driveA", RemoteItem: "itemB", LocalPath: "/mnt/shared/TeamDocs"},
	}

	assert.Equal(t, "your OneDrive account (rate limited)", SKThrottleAccount().Humanize(nil))
	assert.Equal(t, "this drive (rate limited)", SKThrottleDrive(driveid.New("0000000000000001")).Humanize(nil))
	assert.Equal(t, "/mnt/shared/TeamDocs (rate limited)", SKThrottleShared("driveA", "itemB").Humanize(shortcuts))
	assert.Equal(t, "OneDrive service", SKService().Humanize(nil))
	assert.Equal(t, "your OneDrive storage", SKQuotaOwn().Humanize(nil))
	assert.Equal(t, "local disk", SKDiskLocal().Humanize(nil))
	assert.Equal(t, "Documents/Private", SKPermLocalWrite("Documents/Private").Humanize(nil))
	assert.Equal(t, "Shared/TeamDocs", SKPermRemoteWrite("Shared/TeamDocs").Humanize(nil))

	// Shortcut found by local path.
	assert.Equal(t, "/mnt/shared/TeamDocs", SKQuotaShortcut("driveA:itemB").Humanize(shortcuts))

	// Shortcut not found — falls back to composite key.
	assert.Equal(t, "driveX:itemY", SKQuotaShortcut("driveX:itemY").Humanize(nil))
}

// Validates: R-2.10
func TestScopeKey_BlocksTrackedAction(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		key             ScopeKey
		path            string
		oldPath         string
		throttleTarget  string
		shortcutKey     string
		actionType      ActionType
		createSide      FolderCreateSide
		targetsOwnDrive bool
		want            bool
	}{
		// Global scopes block everything.
		{name: "legacy throttle blocks upload", key: SKThrottleAccount(), path: "/a.txt", actionType: ActionUpload, targetsOwnDrive: true, want: true},
		{name: "legacy throttle blocks download", key: SKThrottleAccount(), path: "/a.txt", actionType: ActionDownload, targetsOwnDrive: true, want: true},
		{name: "drive throttle blocks matching drive", key: SKThrottleDrive(driveid.New("0000000000000001")), path: "/a.txt", actionType: ActionUpload, targetsOwnDrive: true, want: true},
		{name: "drive throttle passes other drive", key: SKThrottleDrive(driveid.New("0000000000000001")), path: "/a.txt", actionType: ActionUpload, shortcutKey: "", targetsOwnDrive: false, want: false},
		{name: "shared throttle blocks matching shared target", key: SKThrottleShared("driveA", "itemB"), path: "/a.txt", shortcutKey: "driveA:itemB", actionType: ActionUpload, want: true},
		{name: "shared throttle passes other shared target", key: SKThrottleShared("driveA", "itemB"), path: "/a.txt", shortcutKey: "driveA:itemC", actionType: ActionUpload, want: false},
		{name: "service blocks all", key: SKService(), path: "/a.txt", shortcutKey: "sc:1", actionType: ActionUpload, want: true},

		// Disk:local blocks downloads only.
		{name: "disk blocks download", key: SKDiskLocal(), path: "/a.txt", actionType: ActionDownload, targetsOwnDrive: true, want: true},
		{name: "disk passes upload", key: SKDiskLocal(), path: "/a.txt", actionType: ActionUpload, targetsOwnDrive: true, want: false},

		// Quota:own blocks own-drive uploads.
		{name: "quota own blocks own upload", key: SKQuotaOwn(), path: "/a.txt", actionType: ActionUpload, targetsOwnDrive: true, want: true},
		{name: "quota own passes download", key: SKQuotaOwn(), path: "/a.txt", actionType: ActionDownload, targetsOwnDrive: true, want: false},
		{name: "quota own passes shortcut upload", key: SKQuotaOwn(), path: "/a.txt", shortcutKey: "sc:1", actionType: ActionUpload, want: false},

		// Quota:shortcut blocks matching shortcut uploads.
		{name: "shortcut blocks matching upload", key: SKQuotaShortcut("sc:1"), path: "/a.txt", shortcutKey: "sc:1", actionType: ActionUpload, want: true},
		{name: "shortcut passes wrong shortcut", key: SKQuotaShortcut("sc:1"), path: "/a.txt", shortcutKey: "sc:2", actionType: ActionUpload, want: false},
		{name: "shortcut passes download", key: SKQuotaShortcut("sc:1"), path: "/a.txt", shortcutKey: "sc:1", actionType: ActionDownload, want: false},

		// Local read scopes block uploads that need local readability.
		{name: "perm local read blocks exact dir", key: SKPermLocalRead("Private"), path: "Private", actionType: ActionUpload, targetsOwnDrive: true, want: true},
		{name: "perm local read blocks subpath", key: SKPermLocalRead("Private"), path: "Private/secret.txt", actionType: ActionUpload, targetsOwnDrive: true, want: true},
		{name: "perm local read passes outside", key: SKPermLocalRead("Private"), path: "Public/readme.txt", actionType: ActionUpload, targetsOwnDrive: true, want: false},

		// Perm:remote-write blocks remote mutations recursively, but still allows downloads/local-only work.
		{name: "perm remote blocks upload", key: SKPermRemoteWrite("Shared/TeamDocs"), path: "Shared/TeamDocs/file.txt", actionType: ActionUpload, want: true},
		{name: "perm remote blocks nested remote delete", key: SKPermRemoteWrite("Shared/TeamDocs"), path: "Shared/TeamDocs/nested/file.txt", actionType: ActionRemoteDelete, want: true},
		{name: "perm remote blocks folder create", key: SKPermRemoteWrite("Shared/TeamDocs"), path: "Shared/TeamDocs/newdir", actionType: ActionFolderCreate, createSide: CreateRemote, want: true},
		{name: "perm remote passes download", key: SKPermRemoteWrite("Shared/TeamDocs"), path: "Shared/TeamDocs/file.txt", actionType: ActionDownload, want: false},
		{name: "perm remote passes local delete", key: SKPermRemoteWrite("Shared/TeamDocs"), path: "Shared/TeamDocs/file.txt", actionType: ActionLocalDelete, want: false},
		{name: "perm remote passes outside", key: SKPermRemoteWrite("Shared/TeamDocs"), path: "Shared/Other/file.txt", actionType: ActionUpload, want: false},
	}

	for _, tt := range tests {
		action := Action{
			Type:              tt.actionType,
			Path:              tt.path,
			OldPath:           tt.oldPath,
			CreateSide:        tt.createSide,
			TargetShortcutKey: tt.shortcutKey,
		}
		if tt.targetsOwnDrive {
			action.TargetDriveID = driveid.New("0000000000000001")
		}
		got := tt.key.BlocksTrackedAction(&TrackedAction{Action: action})
		assert.Equal(t, tt.want, got, tt.name)
	}
}

// Validates: R-2.10.1, R-2.10.3
func TestScopeKeyForResult(t *testing.T) {
	t.Parallel()

	assert.Equal(t, SKThrottleDrive(driveid.New("0000000000000001")), ScopeKeyForResult(429, driveid.New("0000000000000001"), ""))
	assert.Equal(t, SKThrottleShared("drive1", "item1"), ScopeKeyForResult(429, driveid.ID{}, "drive1:item1"))
	assert.Equal(t, SKService(), ScopeKeyForResult(503, driveid.ID{}, ""))
	assert.Equal(t, SKService(), ScopeKeyForResult(500, driveid.ID{}, ""))
	assert.Equal(t, SKService(), ScopeKeyForResult(502, driveid.ID{}, ""))
	assert.Equal(t, SKQuotaOwn(), ScopeKeyForResult(507, driveid.ID{}, ""))
	assert.Equal(t, SKQuotaShortcut("drive1:item1"), ScopeKeyForResult(507, driveid.ID{}, "drive1:item1"))
	assert.True(t, ScopeKeyForResult(404, driveid.ID{}, "").IsZero(), "non-scope status should be zero")
	assert.True(t, ScopeKeyForResult(200, driveid.ID{}, "").IsZero(), "success status should be zero")
}
