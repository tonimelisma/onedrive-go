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
	r := ActionCompletion{
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

			result := ss.UpdateScope(&ActionCompletion{
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
	r := ActionCompletion{
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
		r := ActionCompletion{
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

// Validates: R-2.10.3, R-2.10.28, R-2.10.29
func TestScope_5xxSlidingWindow(t *testing.T) {
	t.Parallel()

	clock, advance := controllableClock()
	ss := NewScopeState(clock, discardLogger())

	// Five unique paths with 5xx within 30s must trigger a service block.
	paths := []string{"/a.txt", "/b.txt", "/c.txt", "/d.txt", "/e.txt"}
	for i, p := range paths {
		r := ActionCompletion{
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
		r := ActionCompletion{Path: p, HTTPStatus: 500}
		result := ss.UpdateScope(&r)
		assert.False(t, result.Block)
		advance(1 * time.Second)
	}

	// Advance past the 30s window so the first entries expire.
	advance(30 * time.Second)

	r := ActionCompletion{Path: "/e.txt", HTTPStatus: 500}
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
		r := ActionCompletion{Path: p, HTTPStatus: 507}
		result := ss.UpdateScope(&r)
		assert.False(t, result.Block)
		advance(1 * time.Second)
	}

	// Record a success — this must reset the quota:own window.
	ss.RecordSuccess(&ActionCompletion{Path: "/ok.txt"})

	// Now three more unique failures are needed to trigger the block.
	// The next failure (3rd unique path overall but 1st after reset)
	// must NOT trigger.
	for i, p := range []string{"/c.txt", "/d.txt"} {
		r := ActionCompletion{Path: p, HTTPStatus: 507}
		result := ss.UpdateScope(&r)
		assert.False(t, result.Block, "after reset, path %d should not trigger", i)
		advance(1 * time.Second)
	}

	// Third unique path after reset should trigger.
	r := ActionCompletion{Path: "/e.txt", HTTPStatus: 507}
	result := ss.UpdateScope(&r)
	require.True(t, result.Block, "third unique path after reset should trigger quota:own")
	assert.Equal(t, SKQuotaOwn(), result.ScopeKey)
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
		r := ActionCompletion{
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
			r := ActionCompletion{Path: "/file.txt", HTTPStatus: tc.status}
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
		r := ActionCompletion{Path: p, HTTPStatus: 500}
		result := ss.UpdateScope(&r)
		assert.False(t, result.Block)
		advance(1 * time.Second)
	}

	// Success resets the service window.
	ss.RecordSuccess(&ActionCompletion{Path: "/ok.txt"})

	// Now we need five fresh unique paths to trigger again.
	// The next failure (5th overall but 1st after reset) must not trigger.
	r := ActionCompletion{Path: "/e.txt", HTTPStatus: 500}
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
		{"service", SKService(), "service"},
		{"quota:own", SKQuotaOwn(), "quota:own"},
		{"disk:local", SKDiskLocal(), "disk:local"},
		{"perm:dir", SKPermDir("Documents/Private"), "perm:dir:Documents/Private"},
		{"perm:remote", SKPermRemote("Shared/TeamDocs"), "perm:remote:Shared/TeamDocs"},
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
	assert.False(t, SKPermDir("x").IsZero())
}

// Validates: R-2.10
func TestScopeKey_IsGlobal(t *testing.T) {
	t.Parallel()

	assert.True(t, SKThrottleAccount().IsGlobal())
	assert.False(t, SKThrottleDrive(driveid.New("0000000000000001")).IsGlobal())
	assert.True(t, SKService().IsGlobal())
	assert.False(t, SKQuotaOwn().IsGlobal())
	assert.False(t, SKPermDir("x").IsGlobal())
	assert.False(t, SKPermRemote("x").IsGlobal())
	assert.False(t, SKDiskLocal().IsGlobal())
}

// Validates: R-2.10
func TestScopeKey_IsPermDir(t *testing.T) {
	t.Parallel()

	assert.True(t, SKPermDir("Documents").IsPermDir())
	assert.False(t, SKThrottleAccount().IsPermDir())
	assert.False(t, SKQuotaOwn().IsPermDir())
}

// Validates: R-2.10.34
func TestScopeKey_IsPermRemote(t *testing.T) {
	t.Parallel()

	assert.True(t, SKPermRemote("Shared/TeamDocs").IsPermRemote())
	assert.False(t, SKPermDir("Documents").IsPermRemote())
	assert.False(t, SKThrottleAccount().IsPermRemote())
}

// Validates: R-2.10
func TestScopeKey_DirPath(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "Documents/Private", SKPermDir("Documents/Private").DirPath())

	// DirPath on non-PermDir should panic.
	assert.Panics(t, func() { SKThrottleAccount().DirPath() })
}

// Validates: R-2.10.34
func TestScopeKey_RemotePath(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "Shared/TeamDocs", SKPermRemote("Shared/TeamDocs").RemotePath())
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
		{SKService(), IssueServiceOutage},
		{SKQuotaOwn(), IssueQuotaExceeded},
		{SKPermDir("x"), IssueLocalPermissionDenied},
		{SKPermRemote("Shared/TeamDocs"), IssueSharedFolderBlocked},
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

	assert.Equal(t, "your OneDrive account (rate limited)", SKThrottleAccount().Humanize())
	assert.Equal(t, "this drive (rate limited)", SKThrottleDrive(driveid.New("0000000000000001")).Humanize())
	assert.Equal(t, "OneDrive service", SKService().Humanize())
	assert.Equal(t, "this drive storage", SKQuotaOwn().Humanize())
	assert.Equal(t, "local disk", SKDiskLocal().Humanize())
	assert.Equal(t, "Documents/Private", SKPermDir("Documents/Private").Humanize())
	assert.Equal(t, "Shared/TeamDocs", SKPermRemote("Shared/TeamDocs").Humanize())
}

// Validates: R-2.10
func TestScopeKey_BlocksAction(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		key            ScopeKey
		path           string
		throttleTarget string
		actionType     ActionType
		want           bool
	}{
		// Global scopes block everything.
		{"legacy throttle blocks upload", SKThrottleAccount(), "/a.txt", "", ActionUpload, true},
		{"legacy throttle blocks download", SKThrottleAccount(), "/a.txt", "", ActionDownload, true},
		{"drive throttle blocks matching drive", SKThrottleDrive(driveid.New("0000000000000001")), "/a.txt", "drive:0000000000000001", ActionUpload, true},
		{"drive throttle passes other drive", SKThrottleDrive(driveid.New("0000000000000001")), "/a.txt", "drive:0000000000000002", ActionUpload, false},
		{"service blocks all", SKService(), "/a.txt", "", ActionUpload, true},

		// Disk:local blocks downloads only.
		{"disk blocks download", SKDiskLocal(), "/a.txt", "", ActionDownload, true},
		{"disk passes upload", SKDiskLocal(), "/a.txt", "", ActionUpload, false},

		// Quota blocks uploads regardless of the configured remote root.
		{"quota blocks upload", SKQuotaOwn(), "/a.txt", "", ActionUpload, true},
		{"quota passes download", SKQuotaOwn(), "/a.txt", "", ActionDownload, false},

		// Perm:dir blocks paths under the directory.
		{"perm blocks exact dir", SKPermDir("Private"), "Private", "", ActionUpload, true},
		{"perm blocks subpath", SKPermDir("Private"), "Private/secret.txt", "", ActionUpload, true},
		{"perm passes outside", SKPermDir("Private"), "Public/readme.txt", "", ActionUpload, false},

		// Perm:remote blocks remote mutations recursively, but still allows downloads/local-only work.
		{"perm remote blocks upload", SKPermRemote("Shared/TeamDocs"), "Shared/TeamDocs/file.txt", "", ActionUpload, true},
		{"perm remote blocks nested remote delete", SKPermRemote("Shared/TeamDocs"), "Shared/TeamDocs/nested/file.txt", "", ActionRemoteDelete, true},
		{"perm remote blocks folder create", SKPermRemote("Shared/TeamDocs"), "Shared/TeamDocs/newdir", "", ActionFolderCreate, true},
		{"perm remote passes download", SKPermRemote("Shared/TeamDocs"), "Shared/TeamDocs/file.txt", "", ActionDownload, false},
		{"perm remote passes local delete", SKPermRemote("Shared/TeamDocs"), "Shared/TeamDocs/file.txt", "", ActionLocalDelete, false},
		{"perm remote passes outside", SKPermRemote("Shared/TeamDocs"), "Shared/Other/file.txt", "", ActionUpload, false},
	}

	for _, tt := range tests {
		got := tt.key.BlocksAction(tt.path, tt.throttleTarget, tt.actionType)
		assert.Equal(t, tt.want, got, tt.name)
	}
}

// Validates: R-2.10.1, R-2.10.3
func TestScopeKeyForResult(t *testing.T) {
	t.Parallel()

	assert.Equal(t, SKThrottleDrive(driveid.New("0000000000000001")), ScopeKeyForResult(429, driveid.New("0000000000000001")))
	assert.True(t, ScopeKeyForResult(429, driveid.ID{}).IsZero(), "429 without a target drive should be zero")
	assert.Equal(t, SKService(), ScopeKeyForResult(503, driveid.ID{}))
	assert.Equal(t, SKService(), ScopeKeyForResult(500, driveid.ID{}))
	assert.Equal(t, SKService(), ScopeKeyForResult(502, driveid.ID{}))
	assert.Equal(t, SKQuotaOwn(), ScopeKeyForResult(507, driveid.ID{}))
	assert.True(t, ScopeKeyForResult(404, driveid.ID{}).IsZero(), "non-scope status should be zero")
	assert.True(t, ScopeKeyForResult(200, driveid.ID{}).IsZero(), "success status should be zero")
}
