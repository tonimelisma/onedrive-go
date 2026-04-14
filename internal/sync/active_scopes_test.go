package sync

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
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

// Validates: R-2.10.11, R-2.10.15
func TestFindBlockingScope_GlobalPriorityWins(t *testing.T) {
	t.Parallel()

	blocks := []ScopeBlock{
		{Key: SKService(), IssueType: IssueServiceOutage},
		{Key: SKThrottleAccount(), IssueType: IssueRateLimited},
	}

	got := FindBlockingScope(blocks, makeTrackedAction(ActionUpload, "file.txt"))
	assert.Equal(t, SKThrottleAccount(), got)
}

// Validates: R-2.10.12
func TestFindBlockingScope_PermDirPrefixMatch(t *testing.T) {
	t.Parallel()

	blocks := []ScopeBlock{
		{Key: SKPermLocalWrite("Private"), IssueType: IssueLocalWriteDenied},
	}

	tests := []struct {
		name string
		path string
		want ScopeKey
	}{
		{name: "exact", path: "Private", want: SKPermLocalWrite("Private")},
		{name: "child", path: "Private/sub/file.txt", want: SKPermLocalWrite("Private")},
		{name: "prefix mismatch", path: "PrivateExtra/file.txt", want: ScopeKey{}},
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

	scopeKey := SKPermRemoteWrite("Shared/TeamDocs")
	blocks := []ScopeBlock{
		{Key: scopeKey, IssueType: IssueRemoteWriteDenied},
	}

	tests := []struct {
		name string
		ta   *TrackedAction
		want ScopeKey
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
			want: ScopeKey{},
		},
		{
			name: "outside subtree allowed",
			ta:   makeTrackedAction(ActionUpload, "Shared/Other/file.txt"),
			want: ScopeKey{},
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
		{Key: SKQuotaOwn(), IssueType: IssueQuotaExceeded},
	}

	assert.Equal(t,
		SKQuotaOwn(),
		FindBlockingScope(blocks, makeTrackedAction(ActionUpload, "own.txt")),
	)
	assert.True(t,
		FindBlockingScope(blocks, makeTrackedAction(ActionDownload, "own.txt")).IsZero(),
	)
}

// Validates: R-2.10.9
func TestFindBlockingScope_PrefersMoreSpecificPermissionBoundary(t *testing.T) {
	t.Parallel()

	parent := SKPermRemoteWrite("Shared")
	child := SKPermRemoteWrite("Shared/TeamDocs")
	blocks := []ScopeBlock{
		{Key: parent, IssueType: IssueRemoteWriteDenied},
		{Key: child, IssueType: IssueRemoteWriteDenied},
	}

	got := FindBlockingScope(blocks, makeTrackedAction(ActionUpload, "Shared/TeamDocs/file.txt"))
	assert.Equal(t, child, got, "nested permission scopes should pick the most specific matching boundary")
}

// Validates: R-2.10
func TestUpsertScope_ReplaceAndRemove(t *testing.T) {
	t.Parallel()

	blocks := []ScopeBlock{
		{Key: SKService(), IssueType: IssueServiceOutage},
	}

	updated := UpsertScope(blocks, &ScopeBlock{
		Key:           SKService(),
		IssueType:     IssueServiceOutage,
		TrialInterval: 30 * time.Second,
		TrialCount:    2,
	})

	require.Len(t, updated, 1)
	got, ok := LookupScope(updated, SKService())
	require.True(t, ok)
	assert.Equal(t, 30*time.Second, got.TrialInterval)
	assert.Equal(t, 2, got.TrialCount)

	removed := RemoveScope(updated, SKService())
	assert.False(t, HasScope(removed, SKService()))
}

// Validates: R-2.10.5
func TestExtendScopeTrial(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	blocks := []ScopeBlock{
		{
			Key:           SKThrottleAccount(),
			IssueType:     IssueRateLimited,
			BlockedAt:     now.Add(-time.Minute),
			NextTrialAt:   now.Add(10 * time.Second),
			TrialInterval: 10 * time.Second,
		},
	}

	nextAt := now.Add(30 * time.Second)
	updated, ok := ExtendScopeTrial(blocks, SKThrottleAccount(), nextAt, 20*time.Second)
	require.True(t, ok)

	got, ok := LookupScope(updated, SKThrottleAccount())
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
		{Key: SKThrottleAccount(), NextTrialAt: now.Add(-time.Second)},
		{Key: SKService(), NextTrialAt: now.Add(2 * time.Minute)},
		{Key: SKQuotaOwn()},
	}

	due := DueTrials(blocks, now)
	assert.Equal(t, []ScopeKey{SKThrottleAccount()}, due)

	earliest, ok := EarliestTrialAt(blocks)
	require.True(t, ok)
	assert.Equal(t, now.Add(-time.Second), earliest)
}

// Validates: R-2.10
func TestScopeKeys(t *testing.T) {
	t.Parallel()

	blocks := []ScopeBlock{
		{Key: SKService()},
		{Key: SKThrottleAccount()},
	}

	assert.Equal(t,
		[]ScopeKey{SKService(), SKThrottleAccount()},
		ScopeKeys(blocks),
	)
}

// Validates: R-2.10.43
func TestFindBlockingScope_DiskLocal_DownloadsOnly(t *testing.T) {
	t.Parallel()

	blocks := []ScopeBlock{
		{Key: SKDiskLocal(), IssueType: IssueDiskFull},
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
				assert.Equal(t, SKDiskLocal(), got)
			} else {
				assert.True(t, got.IsZero())
			}
		})
	}
}
