package sync

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

func makeTrackedAction(actionType actionType, path string) *trackedAction {
	return &trackedAction{
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
func TestFindBlockingScope_TargetThrottlePriorityWins(t *testing.T) {
	t.Parallel()

	blocks := []activeScope{
		{Key: SKService()},
		{Key: SKThrottleDrive(driveid.New("d"))},
	}

	got := findBlockingScope(blocks, makeTrackedAction(ActionUpload, "file.txt"))
	assert.Equal(t, SKThrottleDrive(driveid.New("d")), got)
}

// Validates: R-2.10.12
func TestFindBlockingScope_PermDirPrefixMatch(t *testing.T) {
	t.Parallel()

	blocks := []activeScope{
		{Key: SKPermLocalWrite("Private")},
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
			got := findBlockingScope(blocks, makeTrackedAction(ActionDownload, tt.path))
			assert.Equal(t, tt.want, got)
		})
	}
}

// Validates: R-2.10.9, R-2.14.2
func TestFindBlockingScope_PermRemote_IsRecursiveDownloadOnly(t *testing.T) {
	t.Parallel()

	scopeKey := SKPermRemoteWrite("Shared/TeamDocs")
	blocks := []activeScope{
		{Key: scopeKey},
	}

	tests := []struct {
		name string
		ta   *trackedAction
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
			assert.Equal(t, tt.want, findBlockingScope(blocks, tt.ta))
		})
	}
}

// Validates: R-2.10.17, R-2.10.19
func TestFindBlockingScope_QuotaRouting(t *testing.T) {
	t.Parallel()

	blocks := []activeScope{
		{Key: SKQuotaOwn()},
	}

	assert.Equal(t,
		SKQuotaOwn(),
		findBlockingScope(blocks, makeTrackedAction(ActionUpload, "own.txt")),
	)
	assert.True(t,
		findBlockingScope(blocks, makeTrackedAction(ActionDownload, "own.txt")).IsZero(),
	)
}

// Validates: R-2.10.9
func TestFindBlockingScope_PrefersMoreSpecificPermissionBoundary(t *testing.T) {
	t.Parallel()

	parent := SKPermRemoteWrite("Shared")
	child := SKPermRemoteWrite("Shared/TeamDocs")
	blocks := []activeScope{
		{Key: parent},
		{Key: child},
	}

	got := findBlockingScope(blocks, makeTrackedAction(ActionUpload, "Shared/TeamDocs/file.txt"))
	assert.Equal(t, child, got, "nested permission scopes should pick the most specific matching boundary")
}

// Validates: R-2.10.9
func TestFindBlockingScope_MoveSourceInsideBlockedSubtreeBlocksMove(t *testing.T) {
	t.Parallel()

	scopeKey := SKPermRemoteWrite("Shared/Blocked")
	action := makeTrackedAction(ActionRemoteMove, "Shared/Allowed/destination.txt")
	action.Action.OldPath = "Shared/Blocked/source.txt"

	got := findBlockingScope([]activeScope{{Key: scopeKey}}, action)
	assert.Equal(t, scopeKey, got)
}

// Validates: R-2.10
func TestUpsertScope_ReplaceAndRemove(t *testing.T) {
	t.Parallel()

	blocks := []activeScope{
		{Key: SKService()},
	}

	updated := upsertScope(blocks, &activeScope{
		Key:           SKService(),
		TrialInterval: 30 * time.Second,
	})

	require.Len(t, updated, 1)
	got, ok := lookupScope(updated, SKService())
	require.True(t, ok)
	assert.Equal(t, 30*time.Second, got.TrialInterval)

	removed := removeScope(updated, SKService())
	assert.False(t, hasScope(removed, SKService()))
}

// Validates: R-2.10.5
func TestExtendScopeTrial(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	blocks := []activeScope{
		{
			Key:           SKThrottleDrive(driveid.New("d")),
			NextTrialAt:   now.Add(10 * time.Second),
			TrialInterval: 10 * time.Second,
		},
	}

	nextAt := now.Add(30 * time.Second)
	updated, ok := extendScopeTrial(blocks, SKThrottleDrive(driveid.New("d")), nextAt, 20*time.Second)
	require.True(t, ok)

	got, ok := lookupScope(updated, SKThrottleDrive(driveid.New("d")))
	require.True(t, ok)
	assert.Equal(t, nextAt, got.NextTrialAt)
	assert.Equal(t, 20*time.Second, got.TrialInterval)
}

// Validates: R-2.10.5
func TestDueTrialsAndEarliestTrialAt(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	blocks := []activeScope{
		{Key: SKThrottleDrive(driveid.New("d")), NextTrialAt: now.Add(-time.Second)},
		{Key: SKService(), NextTrialAt: now.Add(2 * time.Minute)},
		{Key: SKQuotaOwn()},
	}

	due := dueTrials(blocks, now)
	assert.Equal(t, []ScopeKey{SKThrottleDrive(driveid.New("d"))}, due)

	earliest, ok := earliestTrialAt(blocks)
	require.True(t, ok)
	assert.Equal(t, now.Add(-time.Second), earliest)
}

// Validates: R-2.10
func TestScopeKeys(t *testing.T) {
	t.Parallel()

	blocks := []activeScope{
		{Key: SKService()},
		{Key: SKThrottleDrive(driveid.New("d"))},
	}

	assert.Equal(t,
		[]ScopeKey{SKService(), SKThrottleDrive(driveid.New("d"))},
		scopeKeys(blocks),
	)
}

// Validates: R-2.10.43
func TestFindBlockingScope_DiskLocal_DownloadsOnly(t *testing.T) {
	t.Parallel()

	blocks := []activeScope{
		{Key: SKDiskLocal()},
	}

	tests := []struct {
		actionType  actionType
		wantBlocked bool
	}{
		{actionType: ActionDownload, wantBlocked: true},
		{actionType: ActionUpload, wantBlocked: false},
		{actionType: ActionRemoteDelete, wantBlocked: false},
		{actionType: ActionLocalMove, wantBlocked: false},
	}

	for _, tt := range tests {
		t.Run(tt.actionType.String(), func(t *testing.T) {
			got := findBlockingScope(blocks, makeTrackedAction(tt.actionType, "file.txt"))
			if tt.wantBlocked {
				assert.Equal(t, SKDiskLocal(), got)
			} else {
				assert.True(t, got.IsZero())
			}
		})
	}
}
