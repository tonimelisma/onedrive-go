package sync

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// ---------------------------------------------------------------------------
// Lifecycle tests (Done channel, persistent mode, unknown-ID deadlock guard)
// ---------------------------------------------------------------------------

func TestDepTracker_DoneSignal(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, testLogger(t))

	dt.Add(&Action{
		Type: ActionFolderCreate, Path: "a",
		DriveID: driveid.New("d"), ItemID: "i1",
	}, 1, nil)

	dt.Add(&Action{
		Type: ActionDownload, Path: "b",
		DriveID: driveid.New("d"), ItemID: "i2",
	}, 2, []int64{1})

	// Drain ready channel.
	<-dt.Ready()
	dt.Complete(1)
	<-dt.Ready()
	dt.Complete(2)

	select {
	case <-dt.Done():
		// Success.
	case <-time.After(time.Second):
		require.Fail(t, "timeout waiting for done signal")
	}
}

func TestDepTracker_PersistentMode(t *testing.T) {
	t.Parallel()

	dt := NewPersistentDepTracker(testLogger(t))

	dt.Add(&Action{
		Type: ActionFolderCreate, Path: "dir",
		DriveID: driveid.New("d"), ItemID: "i1",
	}, 1, nil)

	<-dt.Ready()
	dt.Complete(1)

	// In persistent mode, Done() should NOT fire even though all actions
	// are complete. Workers exit via context cancellation instead.
	select {
	case <-dt.Done():
		require.Fail(t, "Done() fired in persistent mode — should never close")
	case <-time.After(100 * time.Millisecond):
		// Expected — Done() never fires in persistent mode.
	}
}

// TestDepTracker_CompleteUnknownID verifies that calling Complete() with an
// unknown ID logs a warning and still increments the completed counter.
// This is a defensive guard against deadlock if the tracker population
// has a subtle bug. Regression test for: silent return without incrementing
// completed → done channel never closed → deadlock.
func TestDepTracker_CompleteUnknownID(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, testLogger(t))

	// Add one real action so total=1.
	dt.Add(&Action{
		Type: ActionFolderCreate, Path: "real",
		DriveID: driveid.New("d"), ItemID: "i1",
	}, 1, nil)

	// Drain the dispatched action.
	<-dt.Ready()

	// Complete with an unknown ID — should log a warning and increment
	// completed. Since total=1 (from Add) and this increments completed to 1,
	// the done channel closes. This verifies the unknown-ID path still
	// advances the completion counter.
	dt.Complete(999)

	select {
	case <-dt.Done():
		// Success — the unknown-ID completion still incremented the counter.
	case <-time.After(time.Second):
		require.Fail(t, "done channel not closed after Complete with unknown ID — deadlock risk")
	}
}

// TestDepTracker_CompleteUnknownID_NoPanic verifies the basic no-panic case
// with zero tracked actions.
func TestDepTracker_CompleteUnknownID_NoPanic(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, testLogger(t))

	// Should not panic on unknown ID with zero total.
	dt.Complete(999)
}

// ---------------------------------------------------------------------------
// Scope gating tests (R-2.10.11, R-2.10.15, R-2.10.5)
// ---------------------------------------------------------------------------

// Validates: R-2.10.11, R-2.10.15
func TestScopeGating_BlockedActionsHeld(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, testLogger(t))

	// Set up a throttle:account scope block — blocks ALL actions.
	block := &ScopeBlock{
		Key:       SKThrottleAccount,
		IssueType: "rate_limited",
		BlockedAt: time.Now(),
	}
	dt.HoldScope(SKThrottleAccount, block)

	// Add an action — it should be diverted to the held queue, not ready.
	dt.Add(&Action{
		Type: ActionUpload, Path: "blocked.txt",
		DriveID: driveid.New("d"), ItemID: "i1",
	}, 1, nil)

	// The ready channel should be empty because the action is held.
	select {
	case <-dt.Ready():
		require.Fail(t, "action matching blocked scope should not appear on ready channel")
	case <-time.After(100 * time.Millisecond):
		// Expected — action is in the held queue.
	}
}

// Validates: R-2.10.15
func TestScopeGating_UnblockedPassthrough(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, testLogger(t))

	// No scope blocks registered — all actions should pass through.
	dt.Add(&Action{
		Type: ActionUpload, Path: "free.txt",
		DriveID: driveid.New("d"), ItemID: "i1",
	}, 1, nil)

	select {
	case ta := <-dt.Ready():
		assert.Equal(t, int64(1), ta.ID, "unblocked action should pass through to ready")
	case <-time.After(time.Second):
		require.Fail(t, "timeout waiting for unblocked action on ready channel")
	}
}

// Validates: R-2.10.11
func TestReleaseScope_DispatchesAllHeld(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, testLogger(t))

	// Block the throttle:account scope.
	block := &ScopeBlock{
		Key:       SKThrottleAccount,
		IssueType: "rate_limited",
		BlockedAt: time.Now(),
	}
	dt.HoldScope(SKThrottleAccount, block)

	// Add three actions — all should be held.
	for i := int64(1); i <= 3; i++ {
		dt.Add(&Action{
			Type:    ActionUpload,
			Path:    fmt.Sprintf("held-%d.txt", i),
			DriveID: driveid.New("d"),
			ItemID:  fmt.Sprintf("i%d", i),
		}, i, nil)
	}

	// Confirm nothing on ready channel.
	select {
	case <-dt.Ready():
		require.Fail(t, "actions should be held, not ready")
	case <-time.After(50 * time.Millisecond):
		// Expected.
	}

	// Release the scope — all held actions should be dispatched.
	dt.ReleaseScope(SKThrottleAccount)

	dispatched := make(map[int64]bool)
	for i := 0; i < 3; i++ {
		select {
		case ta := <-dt.Ready():
			dispatched[ta.ID] = true
		case <-time.After(time.Second):
			require.Fail(t, fmt.Sprintf("timeout waiting for held action %d", i+1))
		}
	}

	assert.Len(t, dispatched, 3, "all three held actions should be dispatched")
	assert.True(t, dispatched[1], "action 1 should be dispatched")
	assert.True(t, dispatched[2], "action 2 should be dispatched")
	assert.True(t, dispatched[3], "action 3 should be dispatched")
}

// Validates: R-2.10.5
func TestDispatchTrial_MarksIsTrial(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, testLogger(t))

	// Block the throttle:account scope.
	block := &ScopeBlock{
		Key:       SKThrottleAccount,
		IssueType: "rate_limited",
		BlockedAt: time.Now(),
	}
	dt.HoldScope(SKThrottleAccount, block)

	// Add an action — it goes to the held queue.
	dt.Add(&Action{
		Type: ActionUpload, Path: "trial.txt",
		DriveID: driveid.New("d"), ItemID: "i1",
	}, 1, nil)

	// Confirm it's held, not ready.
	select {
	case <-dt.Ready():
		require.Fail(t, "action should be held")
	case <-time.After(50 * time.Millisecond):
		// Expected.
	}

	// DispatchTrial should pop from held and mark IsTrial.
	ok := dt.DispatchTrial(SKThrottleAccount)
	require.True(t, ok, "DispatchTrial should return true when held queue is non-empty")

	select {
	case ta := <-dt.Ready():
		assert.True(t, ta.IsTrial, "dispatched trial action should have IsTrial=true")
		assert.Equal(t, int64(1), ta.ID)
	case <-time.After(time.Second):
		require.Fail(t, "timeout waiting for trial action on ready channel")
	}

	// After popping, the held queue should be empty.
	ok = dt.DispatchTrial(SKThrottleAccount)
	assert.False(t, ok, "DispatchTrial should return false when held queue is empty")
}

// ---------------------------------------------------------------------------
// Trial scope key and trial methods (R-2.10.5)
// ---------------------------------------------------------------------------

// Validates: R-2.10.5
func TestDispatchTrial_SetsTrialScopeKey(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, testLogger(t))

	block := &ScopeBlock{
		Key:       SKQuotaOwn,
		IssueType: "quota_exceeded",
		BlockedAt: time.Now(),
	}
	dt.HoldScope(SKQuotaOwn, block)

	dt.Add(&Action{
		Type: ActionUpload, Path: "big.zip",
		DriveID: driveid.New("d"), ItemID: "i1",
	}, 1, nil)

	ok := dt.DispatchTrial(SKQuotaOwn)
	require.True(t, ok)

	select {
	case ta := <-dt.Ready():
		assert.True(t, ta.IsTrial, "should be marked as trial")
		assert.Equal(t, SKQuotaOwn, ta.TrialScopeKey, "should carry scope key")
	case <-time.After(time.Second):
		require.Fail(t, "timeout waiting for trial action")
	}
}

// Validates: R-2.10.5
func TestNextDueTrial(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, testLogger(t))
	now := time.Now()

	// No scope blocks — no due trials.
	key, _, ok := dt.NextDueTrial(now)
	assert.False(t, ok, "no scope blocks → no due trials")
	assert.True(t, key.IsZero())

	// Add a scope block with NextTrialAt in the past.
	block := &ScopeBlock{
		Key:         SKThrottleAccount,
		IssueType:   "rate_limited",
		BlockedAt:   now.Add(-time.Minute),
		NextTrialAt: now.Add(-time.Second),
	}
	dt.HoldScope(SKThrottleAccount, block)

	// Add a held action (NextDueTrial requires a non-empty held queue).
	dt.Add(&Action{
		Type: ActionUpload, Path: "test.txt",
		DriveID: driveid.New("d"), ItemID: "i1",
	}, 1, nil)

	key, trialAt, ok := dt.NextDueTrial(now)
	assert.True(t, ok, "past NextTrialAt with held actions → due trial")
	assert.Equal(t, SKThrottleAccount, key)
	assert.Equal(t, block.NextTrialAt, trialAt)

	// NextTrialAt in the future — not due.
	block.NextTrialAt = now.Add(time.Hour)
	key, _, ok = dt.NextDueTrial(now)
	assert.False(t, ok, "future NextTrialAt → not due")
	assert.True(t, key.IsZero())
}

// Validates: R-2.10.5
func TestExtendTrialInterval(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, testLogger(t))
	now := time.Now()

	block := &ScopeBlock{
		Key:           SKThrottleAccount,
		IssueType:     "rate_limited",
		BlockedAt:     now,
		NextTrialAt:   now.Add(10 * time.Second),
		TrialCount:    0,
		TrialInterval: 10 * time.Second,
	}
	dt.HoldScope(SKThrottleAccount, block)

	newAt := now.Add(30 * time.Second)
	dt.ExtendTrialInterval(SKThrottleAccount, newAt, 20*time.Second)

	// Verify the block was updated via GetScopeBlock (returns a copy).
	updated, ok := dt.GetScopeBlock(SKThrottleAccount)
	require.True(t, ok)

	assert.Equal(t, newAt, updated.NextTrialAt, "NextTrialAt should be extended")
	assert.Equal(t, 1, updated.TrialCount, "TrialCount should be incremented")
	assert.Equal(t, 20*time.Second, updated.TrialInterval, "TrialInterval should be updated")
}

func TestExtendTrialInterval_UnknownScope(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, testLogger(t))

	// Should not panic on unknown scope.
	dt.ExtendTrialInterval(ScopeKey{Kind: ScopeThrottleAccount, Param: "nonexistent"}, time.Now().Add(time.Minute), 30*time.Second)
}

// ---------------------------------------------------------------------------
// EarliestTrialAt (R-2.10.5)
// ---------------------------------------------------------------------------

// Validates: R-2.10.5
func TestEarliestTrialAt_ReturnsEarliest(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, testLogger(t))
	now := time.Now()

	// No blocks → no earliest.
	_, ok := dt.EarliestTrialAt()
	assert.False(t, ok, "no scope blocks → no earliest trial")

	// Add two blocks with different NextTrialAt and held actions.
	dt.HoldScope(SKService, &ScopeBlock{
		Key:         SKService,
		IssueType:   "service_outage",
		NextTrialAt: now.Add(5 * time.Minute),
	})
	dt.Add(&Action{Type: ActionDownload, Path: "a.txt", DriveID: driveid.New("d"), ItemID: "i1"}, 1, nil)

	dt.HoldScope(SKThrottleAccount, &ScopeBlock{
		Key:         SKThrottleAccount,
		IssueType:   "rate_limited",
		NextTrialAt: now.Add(2 * time.Minute),
	})
	dt.Add(&Action{Type: ActionUpload, Path: "b.txt", DriveID: driveid.New("d"), ItemID: "i2"}, 2, nil)

	earliest, ok := dt.EarliestTrialAt()
	assert.True(t, ok)
	assert.Equal(t, now.Add(2*time.Minute), earliest, "should return the earlier of the two")
}

// Validates: R-2.10.5
func TestEarliestTrialAt_SkipsEmptyHeldQueue(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, testLogger(t))
	now := time.Now()

	// Block exists but no held actions.
	dt.HoldScope(SKService, &ScopeBlock{
		Key:         SKService,
		IssueType:   "service_outage",
		NextTrialAt: now.Add(time.Minute),
	})

	_, ok := dt.EarliestTrialAt()
	assert.False(t, ok, "block with no held actions should be skipped")
}

// Validates: R-2.10.5
func TestEarliestTrialAt_SkipsZeroNextTrialAt(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, testLogger(t))

	// Block with zero NextTrialAt.
	dt.HoldScope(SKService, &ScopeBlock{
		Key:       SKService,
		IssueType: "service_outage",
	})
	dt.Add(&Action{Type: ActionDownload, Path: "a.txt", DriveID: driveid.New("d"), ItemID: "i1"}, 1, nil)

	_, ok := dt.EarliestTrialAt()
	assert.False(t, ok, "zero NextTrialAt should be skipped")
}

// ---------------------------------------------------------------------------
// GetScopeBlock
// ---------------------------------------------------------------------------

func TestGetScopeBlock(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, testLogger(t))

	// Unknown key → not found.
	_, ok := dt.GetScopeBlock(ScopeKey{Kind: ScopeThrottleAccount, Param: "nonexistent"})
	assert.False(t, ok)

	// Add a block and retrieve it.
	block := &ScopeBlock{
		Key:           SKQuotaOwn,
		IssueType:     "quota_exceeded",
		TrialInterval: 5 * time.Minute,
	}
	dt.HoldScope(SKQuotaOwn, block)

	got, ok := dt.GetScopeBlock(SKQuotaOwn)
	require.True(t, ok)
	assert.Equal(t, *block, got)

	// GetScopeBlock returns a copy — mutating it must not affect the tracker.
	got.TrialInterval = 99 * time.Hour
	original, ok := dt.GetScopeBlock(SKQuotaOwn)
	require.True(t, ok)
	assert.Equal(t, 5*time.Minute, original.TrialInterval,
		"mutating the returned copy must not affect the tracker's block")
}

// onHeld callback tests (R-2.10.5 — trial timer re-arm gap fix)
// ---------------------------------------------------------------------------

// Validates: R-2.10.5
func TestOnHeldCallback_FiresOnAdd(t *testing.T) {
	t.Parallel()

	logger := testLogger(t)
	dt := NewDepTracker(16, logger)

	var count atomic.Int32
	dt.onHeld = func() { count.Add(1) }

	dt.HoldScope(SKThrottleAccount, &ScopeBlock{
		Key:       SKThrottleAccount,
		IssueType: "rate_limited",
	})

	// Action dispatched with no deps → goes to held → onHeld fires.
	dt.Add(&Action{Type: ActionUpload, Path: "a.txt", DriveID: driveid.New("d"), ItemID: "i1"}, 1, nil)
	assert.Equal(t, int32(1), count.Load(), "onHeld should fire when action enters held queue")
}

func TestOnHeldCallback_NotFiredWhenNotBlocked(t *testing.T) {
	t.Parallel()

	logger := testLogger(t)
	dt := NewDepTracker(16, logger)

	var count atomic.Int32
	dt.onHeld = func() { count.Add(1) }

	// No scope block — action goes to ready.
	dt.Add(&Action{Type: ActionUpload, Path: "a.txt", DriveID: driveid.New("d"), ItemID: "i1"}, 1, nil)
	assert.Equal(t, int32(0), count.Load(), "onHeld should not fire when action goes to ready")
}

// Validates: R-2.10.5
func TestOnHeldCallback_FiresFromComplete(t *testing.T) {
	t.Parallel()

	logger := testLogger(t)
	dt := NewDepTracker(16, logger)

	var count atomic.Int32
	dt.onHeld = func() { count.Add(1) }

	// Add A with no deps (goes to ready), then B depending on A.
	dt.Add(&Action{Type: ActionDownload, Path: "a.txt", DriveID: driveid.New("d"), ItemID: "i1"}, 0, nil)
	dt.Add(&Action{Type: ActionDownload, Path: "b.txt", DriveID: driveid.New("d"), ItemID: "i2"}, 1, []int64{0})

	// Drain A from ready channel.
	<-dt.Ready()

	// Now block the service scope — when A completes, B's deps are
	// satisfied and dispatch sends it to held.
	dt.HoldScope(SKService, &ScopeBlock{
		Key:       SKService,
		IssueType: "service_outage",
	})

	dt.Complete(0) // B becomes ready → blocked by service → held
	assert.Equal(t, int32(1), count.Load(), "onHeld should fire when dependent enters held from Complete")
}

func TestOnHeldCallback_NoDeadlock(t *testing.T) {
	t.Parallel()

	logger := testLogger(t)
	dt := NewDepTracker(16, logger)

	// Set onHeld to call EarliestTrialAt (acquires dt.mu). If the callback
	// were called under the lock, this would self-deadlock.
	dt.onHeld = func() { dt.EarliestTrialAt() }

	dt.HoldScope(SKThrottleAccount, &ScopeBlock{
		Key:           SKThrottleAccount,
		IssueType:     "rate_limited",
		NextTrialAt:   time.Now().Add(time.Minute),
		TrialInterval: time.Minute,
	})

	// Must complete without deadlock.
	done := make(chan struct{})
	go func() {
		dt.Add(&Action{Type: ActionUpload, Path: "a.txt", DriveID: driveid.New("d"), ItemID: "i1"}, 1, nil)
		close(done)
	}()

	select {
	case <-done:
		// OK — no deadlock.
	case <-time.After(3 * time.Second):
		require.Fail(t, "deadlock: onHeld callback must not be called under dt.mu")
	}
}

func TestOnHeldCallback_FiresFromReleaseScope_CrossScope(t *testing.T) {
	t.Parallel()

	logger := testLogger(t)
	dt := NewDepTracker(16, logger)

	var count atomic.Int32
	dt.onHeld = func() { count.Add(1) }

	// Block both throttle:account and service.
	dt.HoldScope(SKThrottleAccount, &ScopeBlock{Key: SKThrottleAccount, IssueType: "rate_limited"})
	dt.HoldScope(SKService, &ScopeBlock{Key: SKService, IssueType: "service_outage"})

	// Add action → goes to held under throttle:account (checked first).
	dt.Add(&Action{Type: ActionDownload, Path: "a.txt", DriveID: driveid.New("d"), ItemID: "i1"}, 1, nil)
	assert.Equal(t, int32(1), count.Load(), "initial add should fire onHeld")

	// Release throttle:account → dispatch re-checks → blocked by service → held again.
	dt.ReleaseScope(SKThrottleAccount)
	assert.Equal(t, int32(2), count.Load(), "cross-scope re-hold should fire onHeld again")
}

// ---------------------------------------------------------------------------

// Validates: R-2.10.12
func TestBlockedScope_PermDir_PathPrefixMatching(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, testLogger(t))

	// Block a directory via perm:dir: scope.
	block := &ScopeBlock{
		Key:       SKPermDir("Private"),
		IssueType: IssueLocalPermissionDenied,
	}
	dt.HoldScope(SKPermDir("Private"), block)

	// An action UNDER the blocked directory should be held.
	dt.Add(&Action{
		Type: ActionDownload, Path: "Private/sub/file.txt",
		DriveID: driveid.New("d"), ItemID: "i1",
	}, 1, nil)

	select {
	case <-dt.Ready():
		require.Fail(t, "action under blocked perm:dir should be held, not dispatched")
	case <-time.After(50 * time.Millisecond):
		// Expected — held.
	}

	// An action OUTSIDE the blocked directory should pass through.
	dt.Add(&Action{
		Type: ActionDownload, Path: "Public/file.txt",
		DriveID: driveid.New("d"), ItemID: "i2",
	}, 2, nil)

	select {
	case ta := <-dt.Ready():
		assert.Equal(t, int64(2), ta.ID, "action outside blocked dir should pass through")
	case <-time.After(time.Second):
		require.Fail(t, "timeout waiting for unblocked action")
	}
}

// Validates: R-2.10.12
func TestBlockedScope_PermDir_ExactMatch(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, testLogger(t))

	block := &ScopeBlock{
		Key:       SKPermDir("Private"),
		IssueType: IssueLocalPermissionDenied,
	}
	dt.HoldScope(SKPermDir("Private"), block)

	// Action at the exact directory path should also be held.
	dt.Add(&Action{
		Type: ActionDownload, Path: "Private",
		DriveID: driveid.New("d"), ItemID: "i1",
	}, 1, nil)

	select {
	case <-dt.Ready():
		require.Fail(t, "action at exact perm:dir path should be held")
	case <-time.After(50 * time.Millisecond):
		// Expected — held.
	}
}

// Validates: R-2.10.12
func TestBlockedScope_PermDir_PrefixMismatch(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, testLogger(t))

	block := &ScopeBlock{
		Key:       SKPermDir("Private"),
		IssueType: IssueLocalPermissionDenied,
	}
	dt.HoldScope(SKPermDir("Private"), block)

	// "PrivateExtra/file.txt" is NOT under "Private" (partial prefix).
	dt.Add(&Action{
		Type: ActionDownload, Path: "PrivateExtra/file.txt",
		DriveID: driveid.New("d"), ItemID: "i1",
	}, 1, nil)

	select {
	case ta := <-dt.Ready():
		assert.Equal(t, int64(1), ta.ID, "partial prefix mismatch should NOT be held")
	case <-time.After(time.Second):
		require.Fail(t, "timeout — partial prefix was incorrectly held")
	}
}

// Validates: R-2.10.38
func TestDiscardScope_CompletesWithoutDispatch(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, testLogger(t))

	// Set a scope block first, then add actions that will be held.
	dt.HoldScope(SKQuotaShortcut("drive1:item1"), &ScopeBlock{
		Key:           SKQuotaShortcut("drive1:item1"),
		TrialInterval: 5 * time.Minute,
	})

	// Add two actions targeting the blocked shortcut — they will be held.
	dt.Add(&Action{
		Path: "/Team Docs/c.txt", Type: ActionUpload,
		DriveID: driveid.New("d"), ItemID: "i3",
		targetShortcutKey: "drive1:item1",
	}, 0, nil)
	dt.Add(&Action{
		Path: "/Team Docs/d.txt", Type: ActionUpload,
		DriveID: driveid.New("d"), ItemID: "i4",
		targetShortcutKey: "drive1:item1",
	}, 0, nil)

	// Both should be held — ready should be empty.
	select {
	case ta := <-dt.Ready():
		require.Fail(t, "expected empty ready channel", "got action %d", ta.ID)
	default:
		// Good — nothing dispatched.
	}

	// Discard the scope — should complete without dispatching.
	dt.DiscardScope(SKQuotaShortcut("drive1:item1"))

	// Verify scope block is gone.
	_, ok := dt.GetScopeBlock(SKQuotaShortcut("drive1:item1"))
	assert.False(t, ok, "scope block should be cleared after discard")

	// Ready channel should still be empty — actions were completed, not dispatched.
	select {
	case ta := <-dt.Ready():
		require.Fail(t, "DiscardScope should NOT dispatch held actions", "got action %d", ta.ID)
	default:
		// Good — nothing dispatched.
	}
}

// Validates: R-2.10.5
// TestDispatchTrial_ClearsNextTrialAt verifies that DispatchTrial clears
// NextTrialAt so the drain loop dispatches only ONE trial per scope per
// tick, not all held actions simultaneously.
func TestDispatchTrial_ClearsNextTrialAt(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, testLogger(t))
	now := time.Now()

	// Block the throttle:account scope with NextTrialAt in the past.
	block := &ScopeBlock{
		Key:           SKThrottleAccount,
		IssueType:     "rate_limited",
		BlockedAt:     now.Add(-time.Minute),
		NextTrialAt:   now.Add(-time.Second),
		TrialInterval: 30 * time.Second,
	}
	dt.HoldScope(SKThrottleAccount, block)

	// Add 5 actions — all should be held.
	for i := int64(1); i <= 5; i++ {
		dt.Add(&Action{
			Type:    ActionUpload,
			Path:    fmt.Sprintf("held-%d.txt", i),
			DriveID: driveid.New("d"),
			ItemID:  fmt.Sprintf("i%d", i),
		}, i, nil)
	}

	// First DispatchTrial should succeed and clear NextTrialAt.
	ok := dt.DispatchTrial(SKThrottleAccount)
	require.True(t, ok, "first DispatchTrial should succeed")

	// After DispatchTrial, NextTrialAt should be zero.
	updated, exists := dt.GetScopeBlock(SKThrottleAccount)
	require.True(t, exists)
	assert.True(t, updated.NextTrialAt.IsZero(),
		"NextTrialAt should be cleared after DispatchTrial to prevent re-dispatch")

	// NextDueTrial should NOT return this scope because NextTrialAt is zero.
	key, _, due := dt.NextDueTrial(now)
	assert.False(t, due, "NextDueTrial should not return scope with zero NextTrialAt")
	assert.True(t, key.IsZero())

	// Drain the dispatched trial.
	select {
	case ta := <-dt.Ready():
		assert.True(t, ta.IsTrial)
	case <-time.After(time.Second):
		require.Fail(t, "timeout waiting for trial action")
	}
}

// Validates: R-2.10.5
// TestDispatchTrial_OnlyOnePerDrainLoop simulates the drain loop pattern and
// verifies that only one trial is dispatched per scope per tick.
func TestDispatchTrial_OnlyOnePerDrainLoop(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, testLogger(t))
	now := time.Now()

	dt.HoldScope(SKThrottleAccount, &ScopeBlock{
		Key:           SKThrottleAccount,
		IssueType:     "rate_limited",
		BlockedAt:     now.Add(-time.Minute),
		NextTrialAt:   now.Add(-time.Second),
		TrialInterval: 30 * time.Second,
	})

	// Add 5 held actions.
	for i := int64(1); i <= 5; i++ {
		dt.Add(&Action{
			Type: ActionUpload, Path: fmt.Sprintf("file-%d.txt", i),
			DriveID: driveid.New("d"), ItemID: fmt.Sprintf("i%d", i),
		}, i, nil)
	}

	// Simulate the drain loop: call NextDueTrial + DispatchTrial in a loop
	// (mirrors drainWorkerResults). Should dispatch exactly 1 trial.
	dispatched := 0
	for {
		key, _, ok := dt.NextDueTrial(now)
		if !ok {
			break
		}

		dt.DispatchTrial(key)
		dispatched++
	}

	assert.Equal(t, 1, dispatched,
		"drain loop should dispatch exactly 1 trial per scope per tick")
}

// TestScopeBlockKeys verifies the ScopeBlockKeys method returns all active
// scope block keys.
func TestScopeBlockKeys(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, testLogger(t))

	// No blocks — should return empty.
	keys := dt.ScopeBlockKeys()
	assert.Empty(t, keys, "no scope blocks → empty keys")

	// Add two blocks.
	dt.HoldScope(SKThrottleAccount, &ScopeBlock{Key: SKThrottleAccount, IssueType: "rate_limited"})
	dt.HoldScope(SKService, &ScopeBlock{Key: SKService, IssueType: "service_outage"})

	keys = dt.ScopeBlockKeys()
	assert.Len(t, keys, 2)
	assert.Contains(t, keys, SKThrottleAccount)
	assert.Contains(t, keys, SKService)

	// Release one — should return only the remaining.
	dt.ReleaseScope(SKThrottleAccount)
	keys = dt.ScopeBlockKeys()
	assert.Len(t, keys, 1)
	assert.Contains(t, keys, SKService)
}

// Validates: R-2.10.38
// Validates: R-2.10.43
func TestBlockedScope_DiskLocal_BlocksDownloadsOnly(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, testLogger(t))

	// Block disk:local scope — should only hold download actions.
	block := &ScopeBlock{
		Key:       SKDiskLocal,
		IssueType: IssueDiskFull,
	}
	dt.HoldScope(SKDiskLocal, block)

	// Download should be held.
	dt.Add(&Action{
		Type: ActionDownload, Path: "file.txt",
		DriveID: driveid.New("d"), ItemID: "i1",
	}, 1, nil)

	select {
	case <-dt.Ready():
		require.Fail(t, "download should be held by disk:local scope")
	case <-time.After(50 * time.Millisecond):
		// Expected — held.
	}

	// Upload should pass through (disk:local only blocks downloads).
	dt.Add(&Action{
		Type: ActionUpload, Path: "upload.txt",
		DriveID: driveid.New("d"), ItemID: "i2",
	}, 2, nil)

	select {
	case ta := <-dt.Ready():
		assert.Equal(t, int64(2), ta.ID, "upload should not be blocked by disk:local")
	case <-time.After(time.Second):
		require.Fail(t, "timeout waiting for upload to pass through disk:local scope")
	}

	// Delete should pass through.
	dt.Add(&Action{
		Type: ActionRemoteDelete, Path: "del.txt",
		DriveID: driveid.New("d"), ItemID: "i3",
	}, 3, nil)

	select {
	case ta := <-dt.Ready():
		assert.Equal(t, int64(3), ta.ID, "delete should not be blocked by disk:local")
	case <-time.After(time.Second):
		require.Fail(t, "timeout waiting for delete to pass through disk:local scope")
	}

	// Move should pass through.
	dt.Add(&Action{
		Type: ActionLocalMove, Path: "moved.txt",
		DriveID: driveid.New("d"), ItemID: "i4",
	}, 4, nil)

	select {
	case ta := <-dt.Ready():
		assert.Equal(t, int64(4), ta.ID, "move should not be blocked by disk:local")
	case <-time.After(time.Second):
		require.Fail(t, "timeout waiting for move to pass through disk:local scope")
	}
}

func TestDiscardScope_NoOpWhenEmpty(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, testLogger(t))

	// Discarding a non-existent scope should not panic.
	dt.DiscardScope(ScopeKey{Kind: ScopeThrottleAccount, Param: "nonexistent"})

	_, ok := dt.GetScopeBlock(ScopeKey{Kind: ScopeThrottleAccount, Param: "nonexistent"})
	assert.False(t, ok)
}

// ---------------------------------------------------------------------------
// ScopeKey.BlocksAction predicate tests — validates each scope key independently
// ---------------------------------------------------------------------------

// TestScopeKey_BlocksAction validates the blocking predicate for each
// fixed-key scope. Table-driven so adding a new scope key only requires
// adding a new test case row.
// TestScopeKey_BlocksAction is in scope_test.go — comprehensive coverage
// for all scope kinds including shortcut and perm:dir.
