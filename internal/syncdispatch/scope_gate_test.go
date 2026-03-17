package syncdispatch

import (
	"context"
	"errors"
	"fmt"
	stdsync "sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// ---------------------------------------------------------------------------
// Mock ScopeBlockStore
// ---------------------------------------------------------------------------

// mockScopeBlockStore is a test double for synctypes.ScopeBlockStore. Tracks calls and
// allows injecting errors.
type mockScopeBlockStore struct {
	blocks      map[string]*synctypes.ScopeBlock // keyed by ScopeKey.String()
	upsertErr   error
	deleteErr   error
	listErr     error
	upsertCalls int
	deleteCalls int
	listCalls   int
}

func newMockScopeBlockStore() *mockScopeBlockStore {
	return &mockScopeBlockStore{
		blocks: make(map[string]*synctypes.ScopeBlock),
	}
}

func (m *mockScopeBlockStore) UpsertScopeBlock(_ context.Context, block *synctypes.ScopeBlock) error {
	m.upsertCalls++
	if m.upsertErr != nil {
		return m.upsertErr
	}

	// Store a copy to prevent aliasing.
	copy := *block
	m.blocks[block.Key.String()] = &copy

	return nil
}

func (m *mockScopeBlockStore) DeleteScopeBlock(_ context.Context, key synctypes.ScopeKey) error {
	m.deleteCalls++
	if m.deleteErr != nil {
		return m.deleteErr
	}

	delete(m.blocks, key.String())
	return nil
}

func (m *mockScopeBlockStore) ListScopeBlocks(_ context.Context) ([]*synctypes.ScopeBlock, error) {
	m.listCalls++
	if m.listErr != nil {
		return nil, m.listErr
	}

	result := make([]*synctypes.ScopeBlock, 0, len(m.blocks))
	for _, b := range m.blocks {
		copy := *b
		result = append(result, &copy)
	}

	return result, nil
}

// newTestScopeGate creates a ScopeGate with a mock store for unit tests.
func newTestScopeGate(t *testing.T) (*ScopeGate, *mockScopeBlockStore) {
	t.Helper()

	store := newMockScopeBlockStore()
	gate := NewScopeGate(store, synctest.TestLogger(t))

	return gate, store
}

// makeTrackedAction creates a minimal synctypes.TrackedAction for Admit tests.
func makeTrackedAction(actionType synctypes.ActionType, path string) *synctypes.TrackedAction {
	return &synctypes.TrackedAction{
		Action: synctypes.Action{
			Type:    actionType,
			Path:    path,
			DriveID: driveid.New("d"),
			ItemID:  "item1",
		},
		ID: 1,
	}
}

// makeShortcutTrackedAction creates a synctypes.TrackedAction targeting a shortcut scope.
func makeShortcutTrackedAction(actionType synctypes.ActionType, path, shortcutKey string) *synctypes.TrackedAction {
	return &synctypes.TrackedAction{
		Action: synctypes.Action{
			Type:              actionType,
			Path:              path,
			DriveID:           driveid.New("d"),
			ItemID:            "item1",
			TargetShortcutKey: shortcutKey,
		},
		ID: 1,
	}
}

// ---------------------------------------------------------------------------
// Admit tests
// ---------------------------------------------------------------------------

// Validates: R-2.10.11, R-2.10.15
func TestScopeGate_Admit_Blocked(t *testing.T) {
	t.Parallel()

	gate, _ := newTestScopeGate(t)
	ctx := context.Background()

	// Set a throttle:account block — blocks ALL actions.
	require.NoError(t, gate.SetScopeBlock(ctx, synctypes.SKThrottleAccount, &synctypes.ScopeBlock{
		Key:       synctypes.SKThrottleAccount,
		IssueType: synctypes.IssueRateLimited,
		BlockedAt: time.Now(),
	}))

	ta := makeTrackedAction(synctypes.ActionUpload, "file.txt")
	key := gate.Admit(ta)
	assert.Equal(t, synctypes.SKThrottleAccount, key, "action matching blocked scope should return the scope key")
}

// Validates: R-2.10
func TestScopeGate_Admit_NotBlocked(t *testing.T) {
	t.Parallel()

	gate, _ := newTestScopeGate(t)

	// No scope blocks — should pass through.
	ta := makeTrackedAction(synctypes.ActionUpload, "file.txt")
	key := gate.Admit(ta)
	assert.True(t, key.IsZero(), "no blocks → zero key")
}

// Validates: R-2.10.26, R-2.10.28
func TestScopeGate_Admit_PriorityOrder(t *testing.T) {
	t.Parallel()

	gate, _ := newTestScopeGate(t)
	ctx := context.Background()

	// Block both throttle:account (global) and service (global).
	// throttle:account should take priority.
	require.NoError(t, gate.SetScopeBlock(ctx, synctypes.SKService, &synctypes.ScopeBlock{
		Key:       synctypes.SKService,
		IssueType: synctypes.IssueServiceOutage,
	}))
	require.NoError(t, gate.SetScopeBlock(ctx, synctypes.SKThrottleAccount, &synctypes.ScopeBlock{
		Key:       synctypes.SKThrottleAccount,
		IssueType: synctypes.IssueRateLimited,
	}))

	ta := makeTrackedAction(synctypes.ActionDownload, "file.txt")
	key := gate.Admit(ta)
	assert.Equal(t, synctypes.SKThrottleAccount, key,
		"throttle:account should be checked before service (priority order)")
}

// Validates: R-2.10.12
func TestScopeGate_Admit_PermDir_PathPrefix(t *testing.T) {
	t.Parallel()

	gate, _ := newTestScopeGate(t)
	ctx := context.Background()

	require.NoError(t, gate.SetScopeBlock(ctx, synctypes.SKPermDir("Private"), &synctypes.ScopeBlock{
		Key:       synctypes.SKPermDir("Private"),
		IssueType: synctypes.IssueLocalPermissionDenied,
	}))

	tests := []struct {
		name        string
		path        string
		wantBlocked bool
	}{
		{"sub-path blocked", "Private/sub/file.txt", true},
		{"exact match blocked", "Private", true},
		{"prefix mismatch not blocked", "PrivateExtra/file.txt", false},
		{"unrelated path not blocked", "Public/file.txt", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ta := makeTrackedAction(synctypes.ActionDownload, tt.path)
			key := gate.Admit(ta)

			if tt.wantBlocked {
				assert.Equal(t, synctypes.SKPermDir("Private"), key, "should be blocked")
			} else {
				assert.True(t, key.IsZero(), "should not be blocked")
			}
		})
	}
}

// Validates: R-2.10.43
func TestScopeGate_Admit_DiskLocal_DownloadsOnly(t *testing.T) {
	t.Parallel()

	gate, _ := newTestScopeGate(t)
	ctx := context.Background()

	require.NoError(t, gate.SetScopeBlock(ctx, synctypes.SKDiskLocal, &synctypes.ScopeBlock{
		Key:       synctypes.SKDiskLocal,
		IssueType: synctypes.IssueDiskFull,
	}))

	tests := []struct {
		actionType  synctypes.ActionType
		wantBlocked bool
	}{
		{synctypes.ActionDownload, true},
		{synctypes.ActionUpload, false},
		{synctypes.ActionRemoteDelete, false},
		{synctypes.ActionLocalMove, false},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("%v", tt.actionType), func(t *testing.T) {
			ta := makeTrackedAction(tt.actionType, "file.txt")
			key := gate.Admit(ta)

			if tt.wantBlocked {
				assert.Equal(t, synctypes.SKDiskLocal, key)
			} else {
				assert.True(t, key.IsZero())
			}
		})
	}
}

// Validates: R-2.10.19
func TestScopeGate_Admit_QuotaOwn(t *testing.T) {
	t.Parallel()

	gate, _ := newTestScopeGate(t)
	ctx := context.Background()

	require.NoError(t, gate.SetScopeBlock(ctx, synctypes.SKQuotaOwn, &synctypes.ScopeBlock{
		Key:       synctypes.SKQuotaOwn,
		IssueType: synctypes.IssueQuotaExceeded,
	}))

	// Own-drive upload — should be blocked.
	ta := makeTrackedAction(synctypes.ActionUpload, "file.txt")
	assert.Equal(t, synctypes.SKQuotaOwn, gate.Admit(ta), "own-drive upload should be blocked by quota:own")

	// Own-drive download — should pass through.
	ta = makeTrackedAction(synctypes.ActionDownload, "file.txt")
	assert.True(t, gate.Admit(ta).IsZero(), "download should not be blocked by quota:own")

	// Shortcut upload — should pass through (different scope).
	ta = makeShortcutTrackedAction(synctypes.ActionUpload, "file.txt", "drive1:item1")
	assert.True(t, gate.Admit(ta).IsZero(), "shortcut upload should not be blocked by quota:own")
}

// Validates: R-2.10.17
func TestScopeGate_Admit_QuotaShortcut(t *testing.T) {
	t.Parallel()

	gate, _ := newTestScopeGate(t)
	ctx := context.Background()

	require.NoError(t, gate.SetScopeBlock(ctx, synctypes.SKQuotaShortcut("drive1:item1"), &synctypes.ScopeBlock{
		Key:       synctypes.SKQuotaShortcut("drive1:item1"),
		IssueType: synctypes.IssueQuotaExceeded,
	}))

	// Shortcut upload matching the key — should be blocked.
	ta := makeShortcutTrackedAction(synctypes.ActionUpload, "Team Docs/file.txt", "drive1:item1")
	assert.Equal(t, synctypes.SKQuotaShortcut("drive1:item1"), gate.Admit(ta),
		"shortcut upload with matching key should be blocked")

	// Different shortcut key — should pass.
	ta = makeShortcutTrackedAction(synctypes.ActionUpload, "Other/file.txt", "drive2:item2")
	assert.True(t, gate.Admit(ta).IsZero(),
		"shortcut upload with different key should not be blocked")

	// Shortcut download — should pass (quota blocks uploads only).
	ta = makeShortcutTrackedAction(synctypes.ActionDownload, "Team Docs/file.txt", "drive1:item1")
	assert.True(t, gate.Admit(ta).IsZero(),
		"shortcut download should not be blocked by quota:shortcut")
}

// ---------------------------------------------------------------------------
// SetScopeBlock / ClearScopeBlock lifecycle tests
// ---------------------------------------------------------------------------

// Validates: R-2.10
func TestScopeGate_SetScopeBlock_Persists(t *testing.T) {
	t.Parallel()

	gate, store := newTestScopeGate(t)
	ctx := context.Background()

	block := &synctypes.ScopeBlock{
		Key:           synctypes.SKThrottleAccount,
		IssueType:     synctypes.IssueRateLimited,
		BlockedAt:     time.Date(2025, 6, 15, 10, 0, 0, 0, time.UTC),
		TrialInterval: 5 * time.Second,
	}

	err := gate.SetScopeBlock(ctx, synctypes.SKThrottleAccount, block)
	require.NoError(t, err)

	// Verify store was called.
	assert.Equal(t, 1, store.upsertCalls, "store.UpsertScopeBlock should be called once")

	// Verify the block is in memory.
	got, ok := gate.GetScopeBlock(synctypes.SKThrottleAccount)
	require.True(t, ok)
	assert.Equal(t, synctypes.IssueRateLimited, got.IssueType)

	// Verify the block is in the mock store.
	require.Contains(t, store.blocks, synctypes.SKThrottleAccount.String())
}

// Validates: R-2.10
func TestScopeGate_SetScopeBlock_StoreError(t *testing.T) {
	t.Parallel()

	gate, store := newTestScopeGate(t)
	ctx := context.Background()

	store.upsertErr = errors.New("disk full")

	block := &synctypes.ScopeBlock{
		Key:       synctypes.SKThrottleAccount,
		IssueType: synctypes.IssueRateLimited,
	}

	err := gate.SetScopeBlock(ctx, synctypes.SKThrottleAccount, block)
	require.Error(t, err, "should return store error")
	assert.Contains(t, err.Error(), "disk full")

	// Memory should be unchanged — block should NOT be in memory.
	_, ok := gate.GetScopeBlock(synctypes.SKThrottleAccount)
	assert.False(t, ok, "memory should not be updated on store error")
}

// Validates: R-2.10
func TestScopeGate_ClearScopeBlock(t *testing.T) {
	t.Parallel()

	gate, store := newTestScopeGate(t)
	ctx := context.Background()

	// Set a block first.
	require.NoError(t, gate.SetScopeBlock(ctx, synctypes.SKService, &synctypes.ScopeBlock{
		Key:       synctypes.SKService,
		IssueType: synctypes.IssueServiceOutage,
	}))

	// Clear it.
	err := gate.ClearScopeBlock(ctx, synctypes.SKService)
	require.NoError(t, err)

	// Verify removed from memory.
	_, ok := gate.GetScopeBlock(synctypes.SKService)
	assert.False(t, ok, "block should be cleared from memory")

	// Verify store.DeleteScopeBlock was called.
	assert.Equal(t, 1, store.deleteCalls)
}

// Validates: R-2.10
func TestScopeGate_ClearScopeBlock_NotFound(t *testing.T) {
	t.Parallel()

	gate, _ := newTestScopeGate(t)
	ctx := context.Background()

	// Clearing a non-existent block should not error.
	err := gate.ClearScopeBlock(ctx, synctypes.SKQuotaOwn)
	require.NoError(t, err)
}

// Validates: R-2.10.5
func TestScopeGate_ClearScopeBlock_StoreError(t *testing.T) {
	t.Parallel()

	gate, store := newTestScopeGate(t)
	ctx := context.Background()

	// Set a block first.
	require.NoError(t, gate.SetScopeBlock(ctx, synctypes.SKService, &synctypes.ScopeBlock{
		Key:       synctypes.SKService,
		IssueType: synctypes.IssueServiceOutage,
	}))

	// Inject store error.
	store.deleteErr = errors.New("store failure")

	// ClearScopeBlock should return the error.
	err := gate.ClearScopeBlock(ctx, synctypes.SKService)
	require.Error(t, err)
	assert.ErrorContains(t, err, "store failure")

	// Verify block still exists in memory (store error prevents removal).
	_, ok := gate.GetScopeBlock(synctypes.SKService)
	assert.True(t, ok, "block should remain in memory on store error")
}

// ---------------------------------------------------------------------------
// IsScopeBlocked
// ---------------------------------------------------------------------------

// Validates: R-2.10
func TestScopeGate_IsScopeBlocked(t *testing.T) {
	t.Parallel()

	gate, _ := newTestScopeGate(t)
	ctx := context.Background()

	assert.False(t, gate.IsScopeBlocked(synctypes.SKThrottleAccount), "should be false when no blocks")

	require.NoError(t, gate.SetScopeBlock(ctx, synctypes.SKThrottleAccount, &synctypes.ScopeBlock{
		Key:       synctypes.SKThrottleAccount,
		IssueType: synctypes.IssueRateLimited,
	}))

	assert.True(t, gate.IsScopeBlocked(synctypes.SKThrottleAccount), "should be true after SetScopeBlock")
	assert.False(t, gate.IsScopeBlocked(synctypes.SKService), "unrelated scope should be false")
}

// ---------------------------------------------------------------------------
// GetScopeBlock
// ---------------------------------------------------------------------------

// Validates: R-2.10
func TestScopeGate_GetScopeBlock_ReturnsCopy(t *testing.T) {
	t.Parallel()

	gate, _ := newTestScopeGate(t)
	ctx := context.Background()

	require.NoError(t, gate.SetScopeBlock(ctx, synctypes.SKQuotaOwn, &synctypes.ScopeBlock{
		Key:           synctypes.SKQuotaOwn,
		IssueType:     synctypes.IssueQuotaExceeded,
		TrialInterval: 5 * time.Minute,
	}))

	got, ok := gate.GetScopeBlock(synctypes.SKQuotaOwn)
	require.True(t, ok)

	// Verify the returned value is a copy by checking two separate calls
	// return equal but independent values.
	got2, ok := gate.GetScopeBlock(synctypes.SKQuotaOwn)
	require.True(t, ok)
	assert.Equal(t, got.TrialInterval, got2.TrialInterval,
		"two GetScopeBlock calls should return the same value")
	assert.Equal(t, 5*time.Minute, got.TrialInterval,
		"returned copy should have the original value")
}

// ---------------------------------------------------------------------------
// ExtendTrialInterval
// ---------------------------------------------------------------------------

// Validates: R-2.10.5
func TestScopeGate_ExtendTrialInterval(t *testing.T) {
	t.Parallel()

	gate, store := newTestScopeGate(t)
	ctx := context.Background()
	now := time.Now()

	require.NoError(t, gate.SetScopeBlock(ctx, synctypes.SKThrottleAccount, &synctypes.ScopeBlock{
		Key:           synctypes.SKThrottleAccount,
		IssueType:     synctypes.IssueRateLimited,
		BlockedAt:     now,
		NextTrialAt:   now.Add(10 * time.Second),
		TrialCount:    0,
		TrialInterval: 10 * time.Second,
	}))

	// Reset upsert counter after initial set.
	store.upsertCalls = 0

	newAt := now.Add(30 * time.Second)
	err := gate.ExtendTrialInterval(ctx, synctypes.SKThrottleAccount, newAt, 20*time.Second)
	require.NoError(t, err)

	// Verify updated in memory.
	updated, ok := gate.GetScopeBlock(synctypes.SKThrottleAccount)
	require.True(t, ok)
	assert.Equal(t, newAt, updated.NextTrialAt, "NextTrialAt should be extended")
	assert.Equal(t, 1, updated.TrialCount, "TrialCount should be incremented")
	assert.Equal(t, 20*time.Second, updated.TrialInterval, "TrialInterval should be updated")

	// Verify persisted.
	assert.Equal(t, 1, store.upsertCalls, "store should be called for persistence")
}

// Validates: R-2.10.5
func TestScopeGate_ExtendTrialInterval_UnknownScope(t *testing.T) {
	t.Parallel()

	gate, store := newTestScopeGate(t)
	ctx := context.Background()

	// Should not error or panic on unknown scope.
	err := gate.ExtendTrialInterval(ctx, synctypes.SKThrottleAccount, time.Now().Add(time.Minute), 30*time.Second)
	require.NoError(t, err)
	assert.Equal(t, 0, store.upsertCalls, "store should not be called for unknown scope")
}

// ---------------------------------------------------------------------------
// AllDueTrials (no held-queue check)
// ---------------------------------------------------------------------------

// Validates: R-2.10.5
func TestScopeGate_AllDueTrials_ReturnsDueScopes(t *testing.T) {
	t.Parallel()

	gate, _ := newTestScopeGate(t)
	ctx := context.Background()
	now := time.Now()

	// Two scopes due (NextTrialAt in the past), one not due (future).
	require.NoError(t, gate.SetScopeBlock(ctx, synctypes.SKThrottleAccount, &synctypes.ScopeBlock{
		Key:         synctypes.SKThrottleAccount,
		IssueType:   synctypes.IssueRateLimited,
		BlockedAt:   now.Add(-time.Minute),
		NextTrialAt: now.Add(-time.Second), // due
	}))
	require.NoError(t, gate.SetScopeBlock(ctx, synctypes.SKService, &synctypes.ScopeBlock{
		Key:         synctypes.SKService,
		IssueType:   synctypes.IssueServiceOutage,
		BlockedAt:   now.Add(-time.Minute),
		NextTrialAt: now.Add(-2 * time.Second), // due
	}))
	require.NoError(t, gate.SetScopeBlock(ctx, synctypes.SKQuotaOwn, &synctypes.ScopeBlock{
		Key:         synctypes.SKQuotaOwn,
		IssueType:   synctypes.IssueQuotaExceeded,
		NextTrialAt: now.Add(time.Hour), // NOT due
	}))

	due := gate.AllDueTrials(now)
	assert.Len(t, due, 2, "should return exactly 2 due scopes")
	assert.Contains(t, due, synctypes.SKThrottleAccount)
	assert.Contains(t, due, synctypes.SKService)
}

// Validates: R-2.10.5
func TestScopeGate_AllDueTrials_SkipsZeroNextTrialAt(t *testing.T) {
	t.Parallel()

	gate, _ := newTestScopeGate(t)
	ctx := context.Background()

	require.NoError(t, gate.SetScopeBlock(ctx, synctypes.SKThrottleAccount, &synctypes.ScopeBlock{
		Key:       synctypes.SKThrottleAccount,
		IssueType: synctypes.IssueRateLimited,
		// NextTrialAt is zero — not trial-eligible.
	}))

	due := gate.AllDueTrials(time.Now())
	assert.Empty(t, due, "zero NextTrialAt should be excluded")
}

// Validates: R-2.10.5
func TestScopeGate_AllDueTrials_NoneReturnsEmpty(t *testing.T) {
	t.Parallel()

	gate, _ := newTestScopeGate(t)
	ctx := context.Background()
	now := time.Now()

	// Only a future scope block — no due trials.
	require.NoError(t, gate.SetScopeBlock(ctx, synctypes.SKThrottleAccount, &synctypes.ScopeBlock{
		Key:         synctypes.SKThrottleAccount,
		IssueType:   synctypes.IssueRateLimited,
		NextTrialAt: now.Add(time.Hour),
	}))

	due := gate.AllDueTrials(now)
	assert.Empty(t, due, "no due scopes → empty slice")
}

// ---------------------------------------------------------------------------
// EarliestTrialAt (no held-queue check)
// ---------------------------------------------------------------------------

// Validates: R-2.10.5
func TestScopeGate_EarliestTrialAt_ReturnsEarliest(t *testing.T) {
	t.Parallel()

	gate, _ := newTestScopeGate(t)
	ctx := context.Background()
	now := time.Now()

	require.NoError(t, gate.SetScopeBlock(ctx, synctypes.SKService, &synctypes.ScopeBlock{
		Key:         synctypes.SKService,
		IssueType:   synctypes.IssueServiceOutage,
		NextTrialAt: now.Add(5 * time.Minute),
	}))
	require.NoError(t, gate.SetScopeBlock(ctx, synctypes.SKThrottleAccount, &synctypes.ScopeBlock{
		Key:         synctypes.SKThrottleAccount,
		IssueType:   synctypes.IssueRateLimited,
		NextTrialAt: now.Add(2 * time.Minute),
	}))

	earliest, ok := gate.EarliestTrialAt()
	assert.True(t, ok)
	assert.Equal(t, now.Add(2*time.Minute), earliest,
		"should return the earlier of the two")
}

// Validates: R-2.10.5
func TestScopeGate_EarliestTrialAt_NoBlocks(t *testing.T) {
	t.Parallel()

	gate, _ := newTestScopeGate(t)

	_, ok := gate.EarliestTrialAt()
	assert.False(t, ok, "no scope blocks → no earliest trial")
}

// Validates: R-2.10.5
func TestScopeGate_EarliestTrialAt_NoHeldQueueCheck(t *testing.T) {
	t.Parallel()

	gate, _ := newTestScopeGate(t)
	ctx := context.Background()
	now := time.Now()

	// Block with NextTrialAt but no held actions — ScopeGate doesn't
	// have a held queue, so this should still return the earliest.
	require.NoError(t, gate.SetScopeBlock(ctx, synctypes.SKService, &synctypes.ScopeBlock{
		Key:         synctypes.SKService,
		IssueType:   synctypes.IssueServiceOutage,
		NextTrialAt: now.Add(time.Minute),
	}))

	earliest, ok := gate.EarliestTrialAt()
	assert.True(t, ok, "ScopeGate.EarliestTrialAt should not check held queue")
	assert.Equal(t, now.Add(time.Minute), earliest)
}

// Validates: R-2.10.5
func TestScopeGate_EarliestTrialAt_SkipsZeroNextTrialAt(t *testing.T) {
	t.Parallel()

	gate, _ := newTestScopeGate(t)
	ctx := context.Background()

	require.NoError(t, gate.SetScopeBlock(ctx, synctypes.SKService, &synctypes.ScopeBlock{
		Key:       synctypes.SKService,
		IssueType: synctypes.IssueServiceOutage,
		// NextTrialAt is zero.
	}))

	_, ok := gate.EarliestTrialAt()
	assert.False(t, ok, "zero NextTrialAt should be skipped")
}

// ---------------------------------------------------------------------------
// ScopeBlockKeys
// ---------------------------------------------------------------------------

// Validates: R-2.10
func TestScopeGate_ScopeBlockKeys(t *testing.T) {
	t.Parallel()

	gate, _ := newTestScopeGate(t)
	ctx := context.Background()

	// No blocks — empty.
	keys := gate.ScopeBlockKeys()
	assert.Empty(t, keys)

	// Add two blocks.
	require.NoError(t, gate.SetScopeBlock(ctx, synctypes.SKThrottleAccount, &synctypes.ScopeBlock{
		Key: synctypes.SKThrottleAccount, IssueType: synctypes.IssueRateLimited,
	}))
	require.NoError(t, gate.SetScopeBlock(ctx, synctypes.SKService, &synctypes.ScopeBlock{
		Key: synctypes.SKService, IssueType: synctypes.IssueServiceOutage,
	}))

	keys = gate.ScopeBlockKeys()
	assert.Len(t, keys, 2)
	assert.Contains(t, keys, synctypes.SKThrottleAccount)
	assert.Contains(t, keys, synctypes.SKService)

	// Clear one.
	require.NoError(t, gate.ClearScopeBlock(ctx, synctypes.SKThrottleAccount))
	keys = gate.ScopeBlockKeys()
	assert.Len(t, keys, 1)
	assert.Contains(t, keys, synctypes.SKService)
}

// ---------------------------------------------------------------------------
// LoadFromStore
// ---------------------------------------------------------------------------

// Validates: R-2.10
func TestScopeGate_LoadFromStore(t *testing.T) {
	t.Parallel()

	store := newMockScopeBlockStore()
	gate := NewScopeGate(store, synctest.TestLogger(t))

	now := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

	// Pre-populate the store (simulating persisted state from a prior run).
	store.blocks[synctypes.SKThrottleAccount.String()] = &synctypes.ScopeBlock{
		Key:           synctypes.SKThrottleAccount,
		IssueType:     synctypes.IssueRateLimited,
		BlockedAt:     now,
		TrialInterval: 10 * time.Second,
		NextTrialAt:   now.Add(10 * time.Second),
		TrialCount:    1,
	}
	store.blocks[synctypes.SKService.String()] = &synctypes.ScopeBlock{
		Key:           synctypes.SKService,
		IssueType:     synctypes.IssueServiceOutage,
		BlockedAt:     now.Add(-time.Minute),
		TrialInterval: 30 * time.Second,
		NextTrialAt:   now.Add(29 * time.Second),
		TrialCount:    2,
	}

	err := gate.LoadFromStore(context.Background())
	require.NoError(t, err)

	// Verify blocks loaded into memory.
	assert.True(t, gate.IsScopeBlocked(synctypes.SKThrottleAccount))
	assert.True(t, gate.IsScopeBlocked(synctypes.SKService))

	got, ok := gate.GetScopeBlock(synctypes.SKThrottleAccount)
	require.True(t, ok)
	assert.Equal(t, synctypes.IssueRateLimited, got.IssueType)
	assert.Equal(t, 1, got.TrialCount)

	assert.Equal(t, 1, store.listCalls, "store.ListScopeBlocks should be called once")
}

// Validates: R-2.10
func TestScopeGate_LoadFromStore_Error(t *testing.T) {
	t.Parallel()

	store := newMockScopeBlockStore()
	store.listErr = errors.New("database locked")
	gate := NewScopeGate(store, synctest.TestLogger(t))

	err := gate.LoadFromStore(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "database locked")
}

// Validates: R-2.10
func TestScopeGate_LoadFromStore_ReplacesExisting(t *testing.T) {
	t.Parallel()

	gate, store := newTestScopeGate(t)
	ctx := context.Background()

	// Set a block in memory.
	require.NoError(t, gate.SetScopeBlock(ctx, synctypes.SKQuotaOwn, &synctypes.ScopeBlock{
		Key:       synctypes.SKQuotaOwn,
		IssueType: synctypes.IssueQuotaExceeded,
	}))

	// Load from store with different blocks — should replace.
	store.blocks = map[string]*synctypes.ScopeBlock{
		synctypes.SKService.String(): {
			Key:       synctypes.SKService,
			IssueType: synctypes.IssueServiceOutage,
		},
	}

	require.NoError(t, gate.LoadFromStore(ctx))

	// Old block should be gone, new block should be present.
	assert.False(t, gate.IsScopeBlocked(synctypes.SKQuotaOwn), "old block should be replaced")
	assert.True(t, gate.IsScopeBlocked(synctypes.SKService), "new block from store should be loaded")
}

// ---------------------------------------------------------------------------
// Concurrency / race detector tests
// ---------------------------------------------------------------------------

// Validates: R-2.10, R-6.4
func TestScopeGate_ConcurrentAdmitDuringSetBlock(t *testing.T) {
	t.Parallel()

	gate, _ := newTestScopeGate(t)
	ctx := context.Background()

	var wg stdsync.WaitGroup

	// Goroutine 1: toggle throttle:account block 100 times.
	wg.Add(1)

	go func() {
		defer wg.Done()

		for range 100 {
			_ = gate.SetScopeBlock(ctx, synctypes.SKThrottleAccount, &synctypes.ScopeBlock{
				Key:       synctypes.SKThrottleAccount,
				IssueType: synctypes.IssueRateLimited,
			})
			_ = gate.ClearScopeBlock(ctx, synctypes.SKThrottleAccount)
		}
	}()

	// Goroutines 2-6: call Admit concurrently.
	for i := range 5 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			ta := makeTrackedAction(synctypes.ActionUpload, fmt.Sprintf("file-%d.txt", i))

			for range 100 {
				gate.Admit(ta)
			}
		}()
	}

	wg.Wait()
}

// Validates: R-2.10.5, R-6.4
func TestScopeGate_ConcurrentExtendAndAllDueTrials(t *testing.T) {
	t.Parallel()

	gate, _ := newTestScopeGate(t)
	ctx := context.Background()
	now := time.Now()

	// Set a scope block with NextTrialAt in the past.
	require.NoError(t, gate.SetScopeBlock(ctx, synctypes.SKThrottleAccount, &synctypes.ScopeBlock{
		Key:           synctypes.SKThrottleAccount,
		IssueType:     synctypes.IssueRateLimited,
		BlockedAt:     now.Add(-time.Minute),
		NextTrialAt:   now.Add(-time.Second),
		TrialInterval: 10 * time.Second,
	}))

	var wg stdsync.WaitGroup
	wg.Add(2)

	// Goroutine 1: extend trial interval 100 times.
	go func() {
		defer wg.Done()

		for i := range 100 {
			nextAt := now.Add(time.Duration(i+1) * time.Second)
			_ = gate.ExtendTrialInterval(ctx, synctypes.SKThrottleAccount, nextAt, time.Duration(i+1)*time.Second)
		}
	}()

	// Goroutine 2: call AllDueTrials 100 times.
	go func() {
		defer wg.Done()

		for range 100 {
			gate.AllDueTrials(now)
		}
	}()

	wg.Wait()
}

// Validates: R-2.10, R-6.4
func TestScopeGate_ConcurrentLoadFromStore(t *testing.T) {
	t.Parallel()

	store := newMockScopeBlockStore()
	gate := NewScopeGate(store, synctest.TestLogger(t))
	ctx := context.Background()
	now := time.Now()

	// Seed the store with 5 scope blocks.
	store.blocks[synctypes.SKThrottleAccount.String()] = &synctypes.ScopeBlock{
		Key: synctypes.SKThrottleAccount, IssueType: synctypes.IssueRateLimited,
		BlockedAt: now, NextTrialAt: now.Add(10 * time.Second),
	}
	store.blocks[synctypes.SKService.String()] = &synctypes.ScopeBlock{
		Key: synctypes.SKService, IssueType: synctypes.IssueServiceOutage,
		BlockedAt: now,
	}
	store.blocks[synctypes.SKDiskLocal.String()] = &synctypes.ScopeBlock{
		Key: synctypes.SKDiskLocal, IssueType: synctypes.IssueDiskFull,
		BlockedAt: now,
	}
	store.blocks[synctypes.SKQuotaOwn.String()] = &synctypes.ScopeBlock{
		Key: synctypes.SKQuotaOwn, IssueType: synctypes.IssueQuotaExceeded,
		BlockedAt: now,
	}
	store.blocks[synctypes.SKPermDir("Private").String()] = &synctypes.ScopeBlock{
		Key: synctypes.SKPermDir("Private"), IssueType: synctypes.IssueLocalPermissionDenied,
		BlockedAt: now,
	}

	var wg stdsync.WaitGroup

	// Goroutine 1: LoadFromStore.
	wg.Add(1)

	go func() {
		defer wg.Done()
		_ = gate.LoadFromStore(ctx)
	}()

	// Goroutines 2-4: concurrent Admit and IsScopeBlocked.
	for i := range 3 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			ta := makeTrackedAction(synctypes.ActionUpload, fmt.Sprintf("file-%d.txt", i))

			for range 50 {
				gate.Admit(ta)
				gate.IsScopeBlocked(synctypes.SKThrottleAccount)
			}
		}()
	}

	wg.Wait()
}
