package sync

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncsched"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// installRetryCacheFromStore installs the run's retry cache from the store,
// as a plan install does, so the invariant compares the two from here on.
func installRetryCacheFromStore(t *testing.T, eng *testEngine, rt *watchRuntime) {
	t.Helper()

	rt.retryWork.install(listRetryWorkForTest(t, eng.baseline, t.Context()))
}

// assertRetryCacheEqualsStore requires the run's retry cache to hold exactly
// the store's rows.
func assertRetryCacheEqualsStore(t *testing.T, eng *testEngine, rt *watchRuntime) {
	t.Helper()

	require.NoError(t, rt.retryWork.matchesStore(listRetryWorkForTest(t, eng.baseline, t.Context())))
}

// The invariant beside the persisted ones compares the cache with the store
// after every completion (mustAssertInvariants); a row written to the store
// behind the cache's back is caught.
//
// Validates: R-2.10.33
func TestEngineFlow_RetryCacheMatchesStoreAfterEveryCompletion(t *testing.T) {
	t.Parallel()

	eng, rt, _, _ := rampTestEngine(t, 2)
	ctx := t.Context()
	installRetryCacheFromStore(t, eng, rt)

	_, err := rt.retryWork.recordFailure(ctx, eng.baseline, &syncdomain.RetryWorkFailure{
		Work: syncfixture.TestRetryWorkKey("failed.txt", "", syncdomain.ActionUpload),
	}, func(int) time.Duration { return time.Minute })
	require.NoError(t, err)
	require.NoError(t, rt.assertCurrentInvariants(ctx), "the cache took the store's row")

	require.NoError(t, eng.baseline.UpsertRetryWork(ctx,
		syncfixture.TestDelayedRetryWorkRow("behind.txt", "", syncdomain.ActionUpload, 1, syncdomain.StoredTimeNano(eng.nowFunc().Add(time.Hour)))))
	require.ErrorContains(t, rt.assertCurrentInvariants(ctx), "retry_work cache")
}

// Discarding a scope deletes only the rows blocked under it. A row that names
// the scope as evidence keeps its attempt count, in the store and the cache;
// the deleted blocked row leaves the cache, and its held action goes back to
// admission as due retry work.
//
// Validates: R-2.10.33, R-2.5.2
func TestEngineFlow_DiscardScope_LeavesNoCachedRowTheStoreDeleted(t *testing.T) {
	t.Parallel()

	eng, rt, _, clock := rampTestEngine(t, 2)
	ctx := t.Context()
	key := syncdomain.SKThrottleDrive(eng.driveID)
	setTestBlockScope(t, eng, &syncdomain.BlockScope{Key: key, TrialInterval: time.Minute})
	held := holdBlockedUploadsForTest(t, eng, rt, key, 1, 1)[0]
	evidence := &syncdomain.RetryWorkFailure{Work: syncfixture.TestRetryWorkKey("evidence.txt", "", syncdomain.ActionUpload), ScopeKey: key}
	delay := func(int) time.Duration { return time.Minute }
	_, err := rt.retryWork.recordFailure(ctx, eng.baseline, evidence, delay)
	require.NoError(t, err)
	installRetryCacheFromStore(t, eng, rt)

	require.NoError(t, rt.discardScope(ctx, key))

	assertRetryCacheEqualsStore(t, eng, rt)
	_, cached := rt.retryWork.row(syncdomain.RetryWorkKeyForAction(&held.Action))
	assert.False(t, cached, "the deleted blocked row left the cache")
	heldNow, ok := rt.sched.HeldWork(syncdomain.RetryWorkKeyForAction(&held.Action))
	require.True(t, ok, "the action is held again, now as retry work")
	assert.Equal(t, syncsched.HeldRetry, heldNow.Reason)
	assert.True(t, heldNow.NextRetry.Equal(clock.Now()), "due at once")

	row, err := rt.retryWork.recordFailure(ctx, eng.baseline, evidence, delay)
	require.NoError(t, err)
	assert.Equal(t, 2, row.AttemptCount, "the evidence row kept its attempt count")
}

// A release batch whose member is no longer blocked in the store, and a key
// the store released with no held action: the cache takes exactly the rows
// the store released.
//
// Validates: R-6.8.24, R-2.10.33
func TestEngineFlow_ReleaseScopeBatch_CacheEqualsStore(t *testing.T) {
	t.Parallel()

	eng, rt, _, _ := rampTestEngine(t, 2)
	ctx := t.Context()
	key := syncdomain.SKThrottleDrive(eng.driveID)
	setTestBlockScope(t, eng, &syncdomain.BlockScope{Key: key, TrialInterval: time.Minute, NextTrialAt: eng.nowFunc().Add(-time.Second)})
	held := holdBlockedUploadsForTest(t, eng, rt, key, 10, 3)
	retryAt := syncdomain.StoredTimeNano(eng.nowFunc().Add(time.Hour))
	require.NoError(t, eng.baseline.UpsertRetryWork(ctx, syncfixture.TestDelayedRetryWorkRow(held[1].Action.Path, "", syncdomain.ActionUpload, 1, retryAt)))
	installRetryCacheFromStore(t, eng, rt)
	scope, active := rt.sched.Scope(key)
	require.True(t, active)

	released, err := rt.releaseScopeBatch(ctx, key, scope.Generation)
	require.NoError(t, err)
	require.NotEmpty(t, released)

	assertRetryCacheEqualsStore(t, eng, rt)
	row, ok := rt.retryWork.row(syncdomain.RetryWorkKeyForAction(&held[1].Action))
	require.True(t, ok)
	assert.False(t, row.Blocked)
	assert.Equal(t, retryAt, row.NextRetryAt, "the row the store did not release keeps its own timing")
}

// An action that succeeds drops its cached row and its hold even when the
// store had no row for it.
//
// Validates: R-2.10.33
func TestEngineFlow_ResolveWithoutStoreRow_ClearsTheCachedRow(t *testing.T) {
	t.Parallel()

	eng, rt, _, _ := rampTestEngine(t, 2)
	ctx := t.Context()
	ta := rt.sched.AddForTest(&syncdomain.Action{Type: syncdomain.ActionUpload, Path: "gone.txt", DriveID: eng.driveID}, 1, nil)
	require.NotNil(t, ta)
	work := syncdomain.RetryWorkKeyForAction(&ta.Action)
	rt.retryWork.rows[work] = *syncfixture.TestDelayedRetryWorkRow(work.Path, "", syncdomain.ActionUpload, 1, syncdomain.StoredTimeNano(eng.nowFunc().Add(time.Minute)))
	rt.sched.Hold(ta, syncsched.HeldRetry, syncdomain.ScopeKey{}, eng.nowFunc().Add(time.Minute))

	require.NoError(t, rt.resolveRetryWorkAndLogResolution(ctx, work, retryResolutionSourceActionSuperseded))

	_, cached := rt.retryWork.row(work)
	assert.False(t, cached, "the cached row is gone")
	assert.False(t, rt.sched.IsHeld(work), "and so is the hold")
}

// retryWorkMutators are the store's methods that change retry_work.
func retryWorkMutators() []string {
	return []string{
		"RecordRetryWorkFailure", "RecordBlockedRetryWork", "ResolveRetryWork", "ClearBlockedRetryWork",
		"ReleaseScopeWork", "DiscardScope", "PruneRetryWorkToCurrentActions", "ReleaseOrphanBlockedRetryWork",
		"UpsertRetryWork", "DeleteRetryWorkByWork",
	}
}

// Only engine_retry_work.go calls a retry_work mutator of the store or writes
// the cache: the package's other non-test sources outside the store's own
// files do neither.
//
// Validates: R-6.3.4
func TestRetryWork_IsTheOnlyEngineWriterOfRetryWork(t *testing.T) {
	t.Parallel()

	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	fset := token.NewFileSet()
	mutators := retryWorkMutators()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") || strings.HasPrefix(name, "store") || name == "engine_retry_work.go" {
			continue
		}
		source, readErr := fs.ReadFile(os.DirFS("."), name)
		require.NoError(t, readErr)
		file, parseErr := parser.ParseFile(fset, name, source, 0)
		require.NoError(t, parseErr)
		ast.Inspect(file, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			for _, mutator := range mutators {
				assert.NotEqual(t, mutator, selector.Sel.Name, "%s calls %s", fset.Position(selector.Pos()), mutator)
			}
			if inner, isSelector := selector.X.(*ast.SelectorExpr); isSelector &&
				inner.Sel.Name == "retryWork" && selector.Sel.Name == "rows" {
				assert.Fail(t, "the retry cache is touched outside engine_retry_work.go", "%s", fset.Position(selector.Pos()))
			}
			return true
		})
	}
}
