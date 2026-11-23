package syncsched

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

func makeTrackedAction(actionType syncdomain.ActionType, path string) *syncdomain.TrackedAction {
	return &syncdomain.TrackedAction{
		Action: syncdomain.Action{
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

	blocks := []syncdomain.ActiveScope{
		{Key: syncdomain.SKService()},
		{Key: syncdomain.SKThrottleDrive(driveid.New("d"))},
	}

	got := FindBlockingScope(blocks, makeTrackedAction(syncdomain.ActionUpload, "file.txt"))
	assert.Equal(t, syncdomain.SKThrottleDrive(driveid.New("d")), got)
}

// Validates: R-2.10.12
func TestFindBlockingScope_PermDirPrefixMatch(t *testing.T) {
	t.Parallel()

	blocks := []syncdomain.ActiveScope{
		{Key: syncdomain.SKPermLocalWrite("Private")},
	}

	tests := []struct {
		name string
		path string
		want syncdomain.ScopeKey
	}{
		{name: "exact", path: "Private", want: syncdomain.SKPermLocalWrite("Private")},
		{name: "child", path: "Private/sub/file.txt", want: syncdomain.SKPermLocalWrite("Private")},
		{name: "prefix mismatch", path: "PrivateExtra/file.txt", want: syncdomain.ScopeKey{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FindBlockingScope(blocks, makeTrackedAction(syncdomain.ActionDownload, tt.path))
			assert.Equal(t, tt.want, got)
		})
	}
}

// Validates: R-2.10.9, R-2.14.2
func TestFindBlockingScope_PermRemote_IsRecursiveDownloadOnly(t *testing.T) {
	t.Parallel()

	scopeKey := syncdomain.SKPermRemoteWrite("Shared/TeamDocs")
	blocks := []syncdomain.ActiveScope{
		{Key: scopeKey},
	}

	tests := []struct {
		name string
		ta   *syncdomain.TrackedAction
		want syncdomain.ScopeKey
	}{
		{
			name: "nested upload blocked",
			ta:   makeTrackedAction(syncdomain.ActionUpload, "Shared/TeamDocs/nested/file.txt"),
			want: scopeKey,
		},
		{
			name: "nested delete blocked",
			ta:   makeTrackedAction(syncdomain.ActionRemoteDelete, "Shared/TeamDocs/nested/file.txt"),
			want: scopeKey,
		},
		{
			name: "download allowed",
			ta:   makeTrackedAction(syncdomain.ActionDownload, "Shared/TeamDocs/nested/file.txt"),
			want: syncdomain.ScopeKey{},
		},
		{
			name: "outside subtree allowed",
			ta:   makeTrackedAction(syncdomain.ActionUpload, "Shared/Other/file.txt"),
			want: syncdomain.ScopeKey{},
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

	blocks := []syncdomain.ActiveScope{
		{Key: syncdomain.SKQuotaOwn()},
	}

	assert.Equal(t,
		syncdomain.SKQuotaOwn(),
		FindBlockingScope(blocks, makeTrackedAction(syncdomain.ActionUpload, "own.txt")),
	)
	assert.True(t,
		FindBlockingScope(blocks, makeTrackedAction(syncdomain.ActionDownload, "own.txt")).IsZero(),
	)
}

// Validates: R-2.10.9
func TestFindBlockingScope_PrefersMoreSpecificPermissionBoundary(t *testing.T) {
	t.Parallel()

	parent := syncdomain.SKPermRemoteWrite("Shared")
	child := syncdomain.SKPermRemoteWrite("Shared/TeamDocs")
	blocks := []syncdomain.ActiveScope{
		{Key: parent},
		{Key: child},
	}

	got := FindBlockingScope(blocks, makeTrackedAction(syncdomain.ActionUpload, "Shared/TeamDocs/file.txt"))
	assert.Equal(t, child, got, "nested permission scopes should pick the most specific matching boundary")
}

// Validates: R-2.10.9
func TestFindBlockingScope_MoveSourceInsideBlockedSubtreeBlocksMove(t *testing.T) {
	t.Parallel()

	scopeKey := syncdomain.SKPermRemoteWrite("Shared/Blocked")
	action := makeTrackedAction(syncdomain.ActionRemoteMove, "Shared/Allowed/destination.txt")
	action.Action.OldPath = "Shared/Blocked/source.txt"

	got := FindBlockingScope([]syncdomain.ActiveScope{{Key: scopeKey}}, action)
	assert.Equal(t, scopeKey, got)
}

// Validates: R-2.10
func TestUpsertScope_ReplaceAndRemove(t *testing.T) {
	t.Parallel()

	blocks := []syncdomain.ActiveScope{
		{Key: syncdomain.SKService()},
	}

	updated := UpsertScope(blocks, &syncdomain.ActiveScope{
		Key:           syncdomain.SKService(),
		TrialInterval: 30 * time.Second,
	})

	require.Len(t, updated, 1)
	got, ok := LookupScope(updated, syncdomain.SKService())
	require.True(t, ok)
	assert.Equal(t, 30*time.Second, got.TrialInterval)

	removed := RemoveScope(updated, syncdomain.SKService())
	assert.False(t, HasScope(removed, syncdomain.SKService()))
}

// Validates: R-2.10.5
func TestExtendScopeTrial(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	blocks := []syncdomain.ActiveScope{
		{
			Key:           syncdomain.SKThrottleDrive(driveid.New("d")),
			NextTrialAt:   now.Add(10 * time.Second),
			TrialInterval: 10 * time.Second,
		},
	}

	nextAt := now.Add(30 * time.Second)
	updated, ok := extendScopeTrial(blocks, syncdomain.SKThrottleDrive(driveid.New("d")), nextAt, 20*time.Second)
	require.True(t, ok)

	got, ok := LookupScope(updated, syncdomain.SKThrottleDrive(driveid.New("d")))
	require.True(t, ok)
	assert.Equal(t, nextAt, got.NextTrialAt)
	assert.Equal(t, 20*time.Second, got.TrialInterval)
}

// Validates: R-2.10.5
func TestDueTrialsAndEarliestTrialAt(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	blocks := []syncdomain.ActiveScope{
		{Key: syncdomain.SKThrottleDrive(driveid.New("d")), NextTrialAt: now.Add(-time.Second)},
		{Key: syncdomain.SKService(), NextTrialAt: now.Add(2 * time.Minute)},
		{Key: syncdomain.SKQuotaOwn()},
	}

	due := dueTrials(blocks, now)
	assert.Equal(t, []syncdomain.ScopeKey{syncdomain.SKThrottleDrive(driveid.New("d"))}, due)

	earliest, ok := earliestTrialAt(blocks)
	require.True(t, ok)
	assert.Equal(t, now.Add(-time.Second), earliest)
}

// Validates: R-2.10
func TestScopeKeys(t *testing.T) {
	t.Parallel()

	blocks := []syncdomain.ActiveScope{
		{Key: syncdomain.SKService()},
		{Key: syncdomain.SKThrottleDrive(driveid.New("d"))},
	}

	assert.Equal(t,
		[]syncdomain.ScopeKey{syncdomain.SKService(), syncdomain.SKThrottleDrive(driveid.New("d"))},
		scopeKeys(blocks),
	)
}

// Validates: R-2.10.43
func TestFindBlockingScope_DiskLocal_DownloadsOnly(t *testing.T) {
	t.Parallel()

	blocks := []syncdomain.ActiveScope{
		{Key: syncdomain.SKDiskLocal()},
	}

	tests := []struct {
		actionType  syncdomain.ActionType
		wantBlocked bool
	}{
		{actionType: syncdomain.ActionDownload, wantBlocked: true},
		{actionType: syncdomain.ActionUpload, wantBlocked: false},
		{actionType: syncdomain.ActionRemoteDelete, wantBlocked: false},
		{actionType: syncdomain.ActionLocalMove, wantBlocked: false},
	}

	for _, tt := range tests {
		t.Run(tt.actionType.String(), func(t *testing.T) {
			got := FindBlockingScope(blocks, makeTrackedAction(tt.actionType, "file.txt"))
			if tt.wantBlocked {
				assert.Equal(t, syncdomain.SKDiskLocal(), got)
			} else {
				assert.True(t, got.IsZero())
			}
		})
	}
}

// dueTrials returns the active scope keys whose trial is due at now. Scopes
// with zero NextTrialAt are excluded.
func dueTrials(blocks []syncdomain.ActiveScope, now time.Time) []syncdomain.ScopeKey {
	var due []syncdomain.ScopeKey

	for i := range blocks {
		if blocks[i].NextTrialAt.IsZero() {
			continue
		}
		if !now.Before(blocks[i].NextTrialAt) {
			due = append(due, blocks[i].Key)
		}
	}

	return due
}

// scopeKeys returns the active scope keys in slice order.
func scopeKeys(blocks []syncdomain.ActiveScope) []syncdomain.ScopeKey {
	keys := make([]syncdomain.ScopeKey, len(blocks))
	for i := range blocks {
		keys[i] = blocks[i].Key
	}
	return keys
}
