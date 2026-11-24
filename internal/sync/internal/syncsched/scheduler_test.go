package syncsched

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncexec"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

const testDriveID = "0000000000000001"

func newTestScheduler(t *testing.T, now time.Time) *Scheduler {
	t.Helper()

	return New(func() time.Time { return now }, synctest.TestLogger(t), nil)
}

func upload(id int64, p string) *syncdomain.TrackedAction {
	return &syncdomain.TrackedAction{ID: id, Action: syncdomain.Action{
		Type: syncdomain.ActionUpload, Path: p, DriveID: driveid.New(testDriveID),
	}}
}

// Installing a plan starts every per-plan structure over: nothing queued,
// running or held, the slow start reset to the new pool size, and the
// active scopes the plan brings. The scope-detection windows and the action
// ID counter outlive the plan.
//
// Validates: R-6.8.25, R-6.8.24
func TestScheduler_InstallReplacesEveryPerPlanStructure(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	s := newTestScheduler(t, now)
	s.Install(8, nil)
	s.NewGraph()
	ids := s.AllocateIDs(3)
	require.Equal(t, []int64{0, 1, 2}, ids)

	queued, running, held := upload(ids[0], "q.txt"), upload(ids[1], "r.txt"), upload(ids[2], "h.txt")
	s.Queued(queued)
	s.Running(running)
	s.Hold(held, HeldScope, syncdomain.SKService(), time.Time{})
	s.PutScope(&syncdomain.ActiveScope{Key: syncdomain.SKService(), NextTrialAt: now})
	s.ObserveFailure(syncexec.CompletionForTest(&syncdomain.ActionCompletion{
		Path: "a.txt", ActionType: syncdomain.ActionUpload, HTTPStatus: 500, Err: assert.AnError,
	}))
	require.True(t, s.HasWindowForTest(syncdomain.SKService()))
	require.Equal(t, 1, s.RunningCount())
	require.Equal(t, 1, s.HeldCount())

	throttle := syncdomain.ActiveScope{Key: syncdomain.SKThrottleDrive(driveid.New(testDriveID)), NextTrialAt: now}
	s.Install(4, []syncdomain.ActiveScope{throttle})

	assert.Zero(t, s.QueuedCount())
	assert.Zero(t, s.RunningCount())
	assert.False(t, s.IsRunning(running.ID))
	assert.Zero(t, s.HeldCount())
	assert.Zero(t, s.HeldUnderScope(syncdomain.SKService()))
	assert.Equal(t, 4, s.ColdStartCeiling())
	assert.Equal(t, []syncdomain.ActiveScope{throttle}, s.Scopes())
	assert.True(t, s.HasWindowForTest(syncdomain.SKService()), "the windows outlive the plan")
	assert.Equal(t, []int64{3}, s.AllocateIDs(1), "action IDs keep counting")

	s.ResetGraph()
	assert.Equal(t, []int64{0}, s.AllocateIDs(1), "a watch's start numbers from zero")
}

// Admission joins the durable retry row and the active scopes: the row's
// scope or retry time holds the action, then an active scope that blocks it
// does, and a trial passes the scope it is the trial of.
//
// Validates: R-2.10.33, R-6.8.24
func TestScheduler_Admit(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	service := syncdomain.SKService()
	quota := syncdomain.SKQuotaOwn()
	moon := time.Date(1969, 7, 20, 21, 17, 0, 0, time.UTC)
	tests := []struct {
		name     string
		trial    syncdomain.ScopeKey
		row      *syncdomain.RetryWorkRow
		active   []syncdomain.ActiveScope
		kind     AdmissionKind
		scope    syncdomain.ScopeKey
		retryAt  time.Time
		clearKey syncdomain.ScopeKey
	}{
		{name: "nothing holds it", kind: AdmitNow},
		{
			name: "a blocked row holds it under its scope", row: &syncdomain.RetryWorkRow{Blocked: true, ScopeKey: quota},
			kind: HoldForScope, scope: quota,
		},
		{
			name: "a trial of the row's scope passes it", trial: quota,
			row: &syncdomain.RetryWorkRow{Blocked: true, ScopeKey: quota}, kind: AdmitNow,
		},
		{
			name: "a future retry time holds it", row: &syncdomain.RetryWorkRow{NextRetryAt: syncdomain.StoredTimeNano(now.Add(time.Minute))},
			kind: HoldForRetry, retryAt: now.Add(time.Minute),
		},
		{
			name: "a past retry time does not", row: &syncdomain.RetryWorkRow{NextRetryAt: syncdomain.StoredTimeNano(now.Add(-time.Minute))},
			kind: AdmitNow,
		},
		{
			name: "an active scope that blocks it holds it", active: []syncdomain.ActiveScope{{Key: service}},
			kind: HoldForScope, scope: service,
		},
		{
			name: "a trial of that scope passes it", trial: service, active: []syncdomain.ActiveScope{{Key: service}},
			kind: AdmitNow,
		},
		{
			name: "a trial of a scope that no longer blocks it clears its row", trial: syncdomain.SKPermRemoteWrite("elsewhere"),
			row:  &syncdomain.RetryWorkRow{Blocked: true, ScopeKey: syncdomain.SKPermRemoteWrite("elsewhere")},
			kind: AdmitNow, clearKey: syncdomain.SKPermRemoteWrite("elsewhere"),
		},
		{
			name: "a retry time before 1970 is still a time", row: &syncdomain.RetryWorkRow{NextRetryAt: syncdomain.StoredTimeNano(moon)},
			kind: AdmitNow,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := newTestScheduler(t, now)
			s.Install(8, tc.active)
			ta := upload(1, "a.txt")
			if !tc.trial.IsZero() {
				ta.IsTrial = true
				ta.TrialScopeKey = tc.trial
			}
			rows := func(syncdomain.RetryWorkKey) (syncdomain.RetryWorkRow, bool) {
				if tc.row == nil {
					return syncdomain.RetryWorkRow{}, false
				}
				return *tc.row, true
			}

			got := s.Admit(now, ta, rows)

			assert.Equal(t, tc.kind, got.Kind)
			assert.Equal(t, tc.scope, got.ScopeKey)
			assert.True(t, tc.retryAt.Equal(got.NextRetryAt), "retry at %v, want %v", got.NextRetryAt, tc.retryAt)
			assert.Equal(t, tc.clearKey, got.ClearScopeKey)
			assert.Equal(t, syncdomain.RetryWorkKeyForAction(&ta.Action), got.RetryWorkKey)
		})
	}
}

// A delayed row timed before 1970 is held until its time, not dispatched at
// once: a negative stored time is a time.
//
// Validates: R-2.10.33
func TestScheduler_ADelayedRowTimedBefore1970IsHeldUntilItsTime(t *testing.T) {
	t.Parallel()

	now := time.Date(1969, 7, 20, 20, 17, 0, 0, time.UTC)
	due := now.Add(time.Hour)
	s := newTestScheduler(t, now)
	held := upload(1, "moon.txt")
	rows := func(syncdomain.RetryWorkKey) (syncdomain.RetryWorkRow, bool) {
		return syncdomain.RetryWorkRow{Path: "moon.txt", AttemptCount: 1, NextRetryAt: syncdomain.StoredTimeNano(due)}, true
	}

	decision := s.Admit(now, held, rows)

	assert.Equal(t, HoldForRetry, decision.Kind, "held as delayed work, not dispatched at once")
	assert.True(t, decision.NextRetryAt.Equal(due))
}

// Held work comes back in order: due retries soonest first, then held
// order; a due trial takes the first action held under its scope; a ramp
// batch is the scope's held work in held order.
//
// Validates: R-2.10.33, R-6.8.24
func TestScheduler_HeldWorkReleasesInOrder(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	s := newTestScheduler(t, now)
	s.Install(8, []syncdomain.ActiveScope{{Key: syncdomain.SKService(), NextTrialAt: now}})
	late, early, same := upload(1, "late.txt"), upload(2, "early.txt"), upload(3, "same.txt")
	s.Hold(late, HeldRetry, syncdomain.ScopeKey{}, now.Add(-time.Second))
	s.Hold(early, HeldRetry, syncdomain.ScopeKey{}, now.Add(-time.Minute))
	s.Hold(same, HeldRetry, syncdomain.ScopeKey{}, now.Add(-time.Second))
	future := upload(4, "future.txt")
	s.Hold(future, HeldRetry, syncdomain.ScopeKey{}, now.Add(time.Hour))
	first, second := upload(5, "first.txt"), upload(6, "second.txt")
	s.Hold(first, HeldScope, syncdomain.SKService(), time.Time{})
	s.Hold(second, HeldScope, syncdomain.SKService(), time.Time{})

	key := func(ta *syncdomain.TrackedAction) syncdomain.RetryWorkKey {
		return syncdomain.RetryWorkKeyForAction(&ta.Action)
	}
	assert.True(t, s.HasDueHeldWork(now))
	assert.Equal(t, []syncdomain.RetryWorkKey{key(early), key(late), key(same)}, s.DueRetries(now))
	assert.Equal(t, []syncdomain.RetryWorkKey{key(first)}, s.DueTrials(now))
	assert.Equal(t, []syncdomain.RetryWorkKey{key(first), key(second)}, s.HeldBatch(syncdomain.SKService(), 5))
	earliest, ok := s.EarliestHeldRetryAt()
	require.True(t, ok)
	assert.True(t, earliest.Equal(now.Add(-time.Minute)))

	assert.Same(t, first, s.Release(key(first)))
	assert.Nil(t, s.Release(key(first)), "released once")
	assert.Equal(t, 1, s.HeldUnderScope(syncdomain.SKService()))
	assert.Equal(t, 5, s.HeldCount())
}

// The owner check runs on every scope accessor and every graph method, so
// a test can prove the scheduler is touched from the run's goroutine only.
//
// Validates: R-6.3.4
func TestScheduler_OwnerCheckRunsOnScopesAndGraph(t *testing.T) {
	t.Parallel()

	s := newTestScheduler(t, time.Now())
	calls := 0
	s.SetOwnerCheck(func() { calls++ })
	s.HasScope(syncdomain.SKService())
	s.InFlight()
	s.NewGraph()
	s.InFlight()

	assert.Equal(t, 3, calls)
}
