package sync

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

func makeTrackedAction(actionType ActionType, path string) *TrackedAction {
	return &TrackedAction{
		Action: Action{
			Type:    actionType,
			Path:    path,
			DriveID: driveid.New("d"),
			ItemID:  "item1",
		},
		ID: 1,
	}
}

func makeShortcutTrackedAction(actionType ActionType, path, shortcutKey string) *TrackedAction {
	return &TrackedAction{
		Action: Action{
			Type:              actionType,
			Path:              path,
			DriveID:           driveid.New("d"),
			ItemID:            "item1",
			TargetShortcutKey: shortcutKey,
		},
		ID: 1,
	}
}

// Validates: R-2.10.11, R-2.10.15
func TestFindBlockingScope_GlobalPriorityWins(t *testing.T) {
	t.Parallel()

	blocks := []ScopeBlock{
		{Key: synctypes.SKService(), IssueType: synctypes.IssueServiceOutage},
		{Key: synctypes.SKThrottleAccount(), IssueType: synctypes.IssueRateLimited},
	}

	got := FindBlockingScope(blocks, makeTrackedAction(ActionUpload, "file.txt"))
	assert.Equal(t, synctypes.SKThrottleAccount(), got)
}

// Validates: R-2.10.12
func TestFindBlockingScope_PermDirPrefixMatch(t *testing.T) {
	t.Parallel()

	blocks := []ScopeBlock{
		{Key: synctypes.SKPermDir("Private"), IssueType: synctypes.IssueLocalPermissionDenied},
	}

	tests := []struct {
		name string
		path string
		want synctypes.ScopeKey
	}{
		{name: "exact", path: "Private", want: synctypes.SKPermDir("Private")},
		{name: "child", path: "Private/sub/file.txt", want: synctypes.SKPermDir("Private")},
		{name: "prefix mismatch", path: "PrivateExtra/file.txt", want: synctypes.ScopeKey{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FindBlockingScope(blocks, makeTrackedAction(ActionDownload, tt.path))
			assert.Equal(t, tt.want, got)
		})
	}
}

// Validates: R-2.10.9, R-2.14.2
func TestFindBlockingScope_PermRemote_IsRecursiveDownloadOnly(t *testing.T) {
	t.Parallel()

	scopeKey := synctypes.SKPermRemote("Shared/TeamDocs")
	blocks := []ScopeBlock{
		{Key: scopeKey, IssueType: synctypes.IssuePermissionDenied},
	}

	tests := []struct {
		name string
		ta   *TrackedAction
		want synctypes.ScopeKey
	}{
		{
			name: "nested upload blocked",
			ta:   makeTrackedAction(ActionUpload, "Shared/TeamDocs/nested/file.txt"),
			want: scopeKey,
		},
		{
			name: "nested delete blocked",
			ta:   makeTrackedAction(ActionRemoteDelete, "Shared/TeamDocs/nested/file.txt"),
			want: scopeKey,
		},
		{
			name: "download allowed",
			ta:   makeTrackedAction(ActionDownload, "Shared/TeamDocs/nested/file.txt"),
			want: synctypes.ScopeKey{},
		},
		{
			name: "outside subtree allowed",
			ta:   makeTrackedAction(ActionUpload, "Shared/Other/file.txt"),
			want: synctypes.ScopeKey{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, FindBlockingScope(blocks, tt.ta))
		})
	}
}

// Validates: R-2.10.17, R-2.10.19
func TestFindBlockingScope_QuotaRouting(t *testing.T) {
	t.Parallel()

	blocks := []ScopeBlock{
		{Key: synctypes.SKQuotaOwn(), IssueType: synctypes.IssueQuotaExceeded},
		{Key: synctypes.SKQuotaShortcut("drive1:item1"), IssueType: synctypes.IssueQuotaExceeded},
	}

	assert.Equal(t,
		synctypes.SKQuotaOwn(),
		FindBlockingScope(blocks, makeTrackedAction(ActionUpload, "own.txt")),
	)
	assert.True(t,
		FindBlockingScope(blocks, makeTrackedAction(ActionDownload, "own.txt")).IsZero(),
	)
	assert.Equal(t,
		synctypes.SKQuotaShortcut("drive1:item1"),
		FindBlockingScope(blocks, makeShortcutTrackedAction(ActionUpload, "Shared/a.txt", "drive1:item1")),
	)
	assert.True(t,
		FindBlockingScope(blocks, makeShortcutTrackedAction(ActionUpload, "Shared/b.txt", "drive2:item2")).IsZero(),
	)
}

// Validates: R-2.10.9
func TestFindBlockingScope_PrefersMoreSpecificPermissionBoundary(t *testing.T) {
	t.Parallel()

	parent := synctypes.SKPermRemote("Shared")
	child := synctypes.SKPermRemote("Shared/TeamDocs")
	blocks := []ScopeBlock{
		{Key: parent, IssueType: synctypes.IssuePermissionDenied},
		{Key: child, IssueType: synctypes.IssuePermissionDenied},
	}

	got := FindBlockingScope(blocks, makeTrackedAction(ActionUpload, "Shared/TeamDocs/file.txt"))
	assert.Equal(t, child, got, "nested permission scopes should pick the most specific matching boundary")
}

// Validates: R-2.10
func TestUpsertScope_ReplaceAndRemove(t *testing.T) {
	t.Parallel()

	blocks := []ScopeBlock{
		{Key: synctypes.SKService(), IssueType: synctypes.IssueServiceOutage},
	}

	updated := UpsertScope(blocks, &ScopeBlock{
		Key:           synctypes.SKService(),
		IssueType:     synctypes.IssueServiceOutage,
		TrialInterval: 30 * time.Second,
		TrialCount:    2,
	})

	require.Len(t, updated, 1)
	got, ok := LookupScope(updated, synctypes.SKService())
	require.True(t, ok)
	assert.Equal(t, 30*time.Second, got.TrialInterval)
	assert.Equal(t, 2, got.TrialCount)

	removed := RemoveScope(updated, synctypes.SKService())
	assert.False(t, HasScope(removed, synctypes.SKService()))
}

// Validates: R-2.10.5
func TestExtendScopeTrial(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	blocks := []ScopeBlock{
		{
			Key:           synctypes.SKThrottleAccount(),
			IssueType:     synctypes.IssueRateLimited,
			BlockedAt:     now.Add(-time.Minute),
			NextTrialAt:   now.Add(10 * time.Second),
			TrialInterval: 10 * time.Second,
		},
	}

	nextAt := now.Add(30 * time.Second)
	updated, ok := ExtendScopeTrial(blocks, synctypes.SKThrottleAccount(), nextAt, 20*time.Second)
	require.True(t, ok)

	got, ok := LookupScope(updated, synctypes.SKThrottleAccount())
	require.True(t, ok)
	assert.Equal(t, nextAt, got.NextTrialAt)
	assert.Equal(t, 20*time.Second, got.TrialInterval)
	assert.Equal(t, 1, got.TrialCount)
}

// Validates: R-2.10.5
func TestDueTrialsAndEarliestTrialAt(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	blocks := []ScopeBlock{
		{Key: synctypes.SKThrottleAccount(), NextTrialAt: now.Add(-time.Second)},
		{Key: synctypes.SKService(), NextTrialAt: now.Add(2 * time.Minute)},
		{Key: synctypes.SKQuotaOwn()},
	}

	due := DueTrials(blocks, now)
	assert.Equal(t, []synctypes.ScopeKey{synctypes.SKThrottleAccount()}, due)

	earliest, ok := EarliestTrialAt(blocks)
	require.True(t, ok)
	assert.Equal(t, now.Add(-time.Second), earliest)
}

// Validates: R-2.10
func TestScopeKeys(t *testing.T) {
	t.Parallel()

	blocks := []ScopeBlock{
		{Key: synctypes.SKService()},
		{Key: synctypes.SKThrottleAccount()},
	}

	assert.Equal(t,
		[]synctypes.ScopeKey{synctypes.SKService(), synctypes.SKThrottleAccount()},
		ScopeKeys(blocks),
	)
}

// Validates: R-2.10.43
func TestFindBlockingScope_DiskLocal_DownloadsOnly(t *testing.T) {
	t.Parallel()

	blocks := []ScopeBlock{
		{Key: synctypes.SKDiskLocal(), IssueType: synctypes.IssueDiskFull},
	}

	tests := []struct {
		actionType  ActionType
		wantBlocked bool
	}{
		{actionType: ActionDownload, wantBlocked: true},
		{actionType: ActionUpload, wantBlocked: false},
		{actionType: ActionRemoteDelete, wantBlocked: false},
		{actionType: ActionLocalMove, wantBlocked: false},
	}

	for _, tt := range tests {
		t.Run(tt.actionType.String(), func(t *testing.T) {
			got := FindBlockingScope(blocks, makeTrackedAction(tt.actionType, "file.txt"))
			if tt.wantBlocked {
				assert.Equal(t, synctypes.SKDiskLocal(), got)
			} else {
				assert.True(t, got.IsZero())
			}
		})
	}
}
