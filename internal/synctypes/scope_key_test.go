package synctypes

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestScopeKey_StringParseRoundTrip(t *testing.T) {
	t.Parallel()

	keys := []ScopeKey{
		SKAuthAccount(),
		SKThrottleAccount(),
		SKService(),
		SKQuotaOwn(),
		SKQuotaShortcut("drive:item"),
		SKPermDir("/docs"),
		SKDiskLocal(),
	}

	for _, key := range keys {
		t.Run(key.String(), func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, key, ParseScopeKey(key.String()))
		})
	}
}

func TestParseScopeKey_UnknownReturnsZero(t *testing.T) {
	t.Parallel()

	assert.True(t, ParseScopeKey("not-a-scope").IsZero())
}

func TestScopeKey_IsGlobal(t *testing.T) {
	t.Parallel()

	assert.True(t, SKAuthAccount().IsGlobal())
	assert.True(t, SKThrottleAccount().IsGlobal())
	assert.True(t, SKService().IsGlobal())
	assert.False(t, SKQuotaOwn().IsGlobal())
}

func TestScopeKey_IsPermDirAndDirPath(t *testing.T) {
	t.Parallel()

	key := SKPermDir("/docs")
	assert.True(t, key.IsPermDir())
	assert.Equal(t, "/docs", key.DirPath())
}

func TestScopeKey_IsPermRemoteAndRemotePath(t *testing.T) {
	t.Parallel()

	key := SKPermRemote("/readonly")
	assert.True(t, key.IsPermRemote())
	assert.Equal(t, "/readonly", key.RemotePath())
}

func TestScopeKey_DirPathPanicsForNonPermDir(t *testing.T) {
	t.Parallel()

	require.Panics(t, func() {
		_ = SKQuotaOwn().DirPath()
	})
}

func TestScopeKey_RemotePathPanicsForNonPermRemote(t *testing.T) {
	t.Parallel()

	require.Panics(t, func() {
		_ = SKQuotaOwn().RemotePath()
	})
}

func TestScopeKey_IssueType(t *testing.T) {
	t.Parallel()

	assert.Equal(t, IssueUnauthorized, SKAuthAccount().IssueType())
	assert.Equal(t, IssueRateLimited, SKThrottleAccount().IssueType())
	assert.Equal(t, IssueServiceOutage, SKService().IssueType())
	assert.Equal(t, IssueQuotaExceeded, SKQuotaOwn().IssueType())
	assert.Equal(t, IssueQuotaExceeded, SKQuotaShortcut("drive:item").IssueType())
	assert.Equal(t, IssueLocalPermissionDenied, SKPermDir("/docs").IssueType())
	assert.Equal(t, IssueSharedFolderBlocked, SKPermRemote("/readonly").IssueType())
	assert.Equal(t, IssueDiskFull, SKDiskLocal().IssueType())
	assert.Empty(t, ScopeKey{}.IssueType())
}

func TestScopeKey_Humanize(t *testing.T) {
	t.Parallel()

	shortcuts := []Shortcut{{
		RemoteDrive: "drive",
		RemoteItem:  "item",
		LocalPath:   "Team Docs",
	}}

	assert.Equal(t, "your OneDrive account authorization", SKAuthAccount().Humanize(shortcuts))
	assert.Equal(t, "your OneDrive account (rate limited)", SKThrottleAccount().Humanize(shortcuts))
	assert.Equal(t, "OneDrive service", SKService().Humanize(shortcuts))
	assert.Equal(t, "your OneDrive storage", SKQuotaOwn().Humanize(shortcuts))
	assert.Equal(t, "Team Docs", SKQuotaShortcut("drive:item").Humanize(shortcuts))
	assert.Equal(t, "missing:item", SKQuotaShortcut("missing:item").Humanize(shortcuts))
	assert.Equal(t, "/docs", SKPermDir("/docs").Humanize(shortcuts))
	assert.Equal(t, "local disk", SKDiskLocal().Humanize(shortcuts))
}

func TestScopeKey_BlocksAction(t *testing.T) {
	t.Parallel()

	assert.True(t, SKAuthAccount().BlocksAction("/docs/file.txt", "", ActionDownload, false))
	assert.True(t, SKAuthAccount().BlocksAction("/docs/file.txt", "", ActionUpload, true))
	assert.True(t, SKThrottleAccount().BlocksAction("/docs/file.txt", "", ActionDownload, false))
	assert.True(t, SKService().BlocksAction("/docs/file.txt", "", ActionUpload, true))
	assert.True(t, SKDiskLocal().BlocksAction("/docs/file.txt", "", ActionDownload, false))
	assert.False(t, SKDiskLocal().BlocksAction("/docs/file.txt", "", ActionUpload, false))
	assert.True(t, SKQuotaOwn().BlocksAction("/docs/file.txt", "", ActionUpload, true))
	assert.False(t, SKQuotaOwn().BlocksAction("/docs/file.txt", "", ActionUpload, false))
	assert.True(t, SKQuotaShortcut("drive:item").BlocksAction("/docs/file.txt", "drive:item", ActionUpload, false))
	assert.False(t, SKQuotaShortcut("drive:item").BlocksAction("/docs/file.txt", "other:item", ActionUpload, false))
	assert.True(t, SKPermDir("/docs").BlocksAction("/docs/file.txt", "", ActionUpload, false))
	assert.True(t, SKPermDir("/docs").BlocksAction("/docs", "", ActionUpload, false))
	assert.False(t, SKPermDir("/docs").BlocksAction("/other/file.txt", "", ActionUpload, false))
	assert.True(t, SKPermRemote("/readonly").BlocksAction("/readonly/file.txt", "", ActionUpload, false))
	assert.True(t, SKPermRemote("/readonly").BlocksAction("/readonly/file.txt", "", ActionRemoteDelete, false))
	assert.True(t, SKPermRemote("/readonly").BlocksAction("/readonly", "", ActionFolderCreate, false))
	assert.False(t, SKPermRemote("/readonly").BlocksAction("/readonly/file.txt", "", ActionDownload, false))
	assert.False(t, SKPermRemote("/readonly").BlocksAction("/readonly/file.txt", "", ActionLocalDelete, false))
	assert.False(t, SKPermRemote("/readonly").BlocksAction("/other/file.txt", "", ActionUpload, false))
}

func TestScopeKeyForStatus(t *testing.T) {
	t.Parallel()

	assert.Equal(t, SKThrottleAccount(), ScopeKeyForStatus(http.StatusTooManyRequests, ""))
	assert.Equal(t, SKService(), ScopeKeyForStatus(http.StatusServiceUnavailable, ""))
	assert.Equal(t, SKQuotaOwn(), ScopeKeyForStatus(http.StatusInsufficientStorage, ""))
	assert.Equal(t, SKQuotaShortcut("drive:item"), ScopeKeyForStatus(http.StatusInsufficientStorage, "drive:item"))
	assert.Equal(t, SKService(), ScopeKeyForStatus(http.StatusBadGateway, ""))
	assert.True(t, ScopeKeyForStatus(http.StatusOK, "").IsZero())
}
