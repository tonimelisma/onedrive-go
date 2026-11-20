package sync

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/authstate"
	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncexec"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// Validates: R-6.8.9
func TestOneShotEngineLoop_ClosedResultsStillProcessBufferedRetryWork(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &syncfixture.EngineMockClient{})
	runner := newOneShotRunner(eng.Engine)
	runner.sched.graph = syncexec.NewDepGraph(eng.logger)
	runner.sched.dispatchCh = make(chan *syncdomain.TrackedAction, 16)

	for _, actionID := range []int64{1, 2, 3} {
		runner.sched.graph.Add(&syncdomain.Action{
			Path: "action.txt",
			Type: syncdomain.ActionUpload,
		}, actionID, nil)
	}

	results := make(chan syncdomain.ActionCompletion, 3)
	results <- syncdomain.ActionCompletion{
		ActionID:   1,
		Path:       "a.txt",
		ActionType: syncdomain.ActionUpload,
		HTTPStatus: http.StatusBadGateway,
		Err:        graph.ErrServerError,
		ErrMsg:     "fail-1",
	}
	results <- syncdomain.ActionCompletion{
		ActionID:   2,
		Path:       "b.txt",
		ActionType: syncdomain.ActionUpload,
		HTTPStatus: http.StatusBadGateway,
		Err:        graph.ErrServerError,
		ErrMsg:     "fail-2",
	}
	results <- syncdomain.ActionCompletion{
		ActionID:   3,
		Path:       "c.txt",
		ActionType: syncdomain.ActionDownload,
		Success:    true,
	}
	close(results)

	err := syncfixture.RunWithin(t, t.Context(), "the results loop to finish its closed channel", func(ctx context.Context) error {
		return runner.runResultsLoopWithInitialOutbox(ctx, nil, nil, results, nil)
	})
	require.NoError(t, err)

	retryRows := listRetryWorkForTest(t, eng.baseline, t.Context())
	require.Len(t, retryRows, 2)
	assert.ElementsMatch(t, []string{"a.txt", "b.txt"}, []string{retryRows[0].Path, retryRows[1].Path})
}

// Validates: R-2.10.5
func TestOneShotEngineLoop_UnauthorizedTerminatesAndDrainsQueuedReady(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &syncfixture.EngineMockClient{})
	runner := newOneShotRunner(eng.Engine)
	runner.sched.graph = syncexec.NewDepGraph(eng.logger)
	runner.sched.dispatchCh = make(chan *syncdomain.TrackedAction)

	runner.sched.graph.Add(&syncdomain.Action{
		Type: syncdomain.ActionUpload,
		Path: "root.txt",
		View: &syncdomain.PathView{Path: "root.txt"},
	}, 1, nil)
	runner.sched.graph.Add(&syncdomain.Action{
		Type: syncdomain.ActionUpload,
		Path: "child.txt",
		View: &syncdomain.PathView{Path: "child.txt"},
	}, 2, []int64{1})
	runner.sched.graph.Add(&syncdomain.Action{
		Type: syncdomain.ActionDownload,
		Path: "auth.txt",
		View: &syncdomain.PathView{Path: "auth.txt"},
	}, 3, nil)

	results := make(chan syncdomain.ActionCompletion, 2)
	results <- syncdomain.ActionCompletion{
		ActionID:   1,
		Path:       "root.txt",
		ActionType: syncdomain.ActionUpload,
		Success:    true,
	}
	results <- syncdomain.ActionCompletion{
		ActionID:   3,
		Path:       "auth.txt",
		ActionType: syncdomain.ActionDownload,
		HTTPStatus: http.StatusUnauthorized,
		Err:        graph.ErrUnauthorized,
		ErrMsg:     "unauthorized",
	}
	close(results)

	err := syncfixture.RunWithin(t, t.Context(), "the results loop to stop on the unauthorized completion", func(ctx context.Context) error {
		return runner.runResultsLoopWithInitialOutbox(ctx, nil, nil, results, nil)
	})
	require.ErrorIs(t, err, graph.ErrUnauthorized)
	assert.Equal(t, 0, runner.sched.graph.InFlightCount())
	assert.Empty(t, listRetryWorkForTest(t, eng.baseline, t.Context()))
	assert.Empty(t, actionableObservationIssuesForTest(t, eng.baseline, t.Context()))

	required, authErr := eng.hasPersistedAccountAuthRequirement()
	require.NoError(t, authErr)
	assert.True(t, required)
}

// signInRequirementOf is the sign-in requirement the catalog of eng's data
// directory holds for account.
func signInRequirementOf(t *testing.T, eng *testEngine, account driveid.CanonicalID) authstate.Reason {
	t.Helper()

	catalog, err := config.LoadCatalogForDataDir(eng.dataDir)
	require.NoError(t, err)
	record, found := catalog.AccountByCanonicalID(account)
	require.True(t, found, "the catalog knows %s", account)

	return record.AuthRequirementReason
}

// Validates: R-2.10.45, R-2.10.46
//
// A personal and a work or school account can share an address. A sync's 401
// marks the account the engine syncs with, found by its canonical ID,
// whichever of the two sorts first. The next startup proof ends that
// account's requirement alone, and the other account's requirement neither
// makes this engine revalidate nor ends with it.
func TestRunOnce_TheSignInRequirementIsTheSyncedAccountsAloneOfTwoAtOneAddress(t *testing.T) {
	t.Parallel()

	personal := testEngineAccountCanonicalID()
	business := driveid.MustCanonicalID(driveid.DriveTypeBusiness + ":" + engineTestEmail)

	for _, pair := range [][2]driveid.CanonicalID{{personal, business}, {business, personal}} {
		synced, other := pair[0], pair[1]
		t.Run(synced.DriveType(), func(t *testing.T) {
			t.Parallel()

			mock := newRunOnceBidirectionalMock(driveid.New(syncfixture.EngineTestDriveID))
			upload := mock.UploadFn
			mock.UploadFn = func(
				context.Context, driveid.ID, string, string, io.ReaderAt, int64, time.Time, graph.ProgressFunc,
			) (*graph.Item, error) {
				return nil, syncfixture.GraphStatusError(http.StatusUnauthorized, graph.ErrUnauthorized)
			}
			eng, syncRoot := newTestEngineInDirWith(t, t.Context(), mock, t.TempDir(), func(inputs *engineInputs) {
				inputs.AccountCanonicalID = synced
				inputs.DriveVerifier = mock
			})
			seedTestEngineAccount(t, eng.dataDir, other)
			syncfixture.WriteLocalFile(t, syncRoot, "local.txt", "local content\n")

			_, err := eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{})
			require.ErrorIs(t, err, graph.ErrUnauthorized)
			assert.Equal(t, authstate.ReasonSyncAuthRejected, signInRequirementOf(t, eng, synced), "the 401 marks the synced account")
			assert.Empty(t, signInRequirementOf(t, eng, other), "and not the other account at its address")

			require.NoError(t, config.MarkAccountAuthRequired(eng.dataDir, other, authstate.ReasonSyncAuthRejected))
			mock.UploadFn = upload
			_, err = eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{})
			require.NoError(t, err, "the startup proof revalidates the synced account")
			assert.Empty(t, signInRequirementOf(t, eng, synced), "and ends its requirement")
			assert.Equal(t, authstate.ReasonSyncAuthRejected, signInRequirementOf(t, eng, other), "the other account's stands")

			required, err := eng.hasPersistedAccountAuthRequirement()
			require.NoError(t, err)
			assert.False(t, required, "the other account's requirement is not this engine's to revalidate")
		})
	}
}

// Validates: R-2.8.7, R-2.10.5
func TestOneShotEngineLoop_SupersededCompletionRetiresDependentsWithoutSuccessOrRetry(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &syncfixture.EngineMockClient{})
	runner := newOneShotRunner(eng.Engine)
	runner.sched.graph = syncexec.NewDepGraph(eng.logger)
	runner.sched.dispatchCh = make(chan *syncdomain.TrackedAction, 1)

	root := runner.sched.graph.Add(&syncdomain.Action{
		Type: syncdomain.ActionUpload,
		Path: "root.txt",
		View: &syncdomain.PathView{Path: "root.txt"},
	}, 1, nil)
	require.NotNil(t, root)
	runner.sched.markRunning(root)

	child := runner.sched.graph.Add(&syncdomain.Action{
		Type: syncdomain.ActionDownload,
		Path: "child.txt",
	}, 2, []int64{1})
	assert.Nil(t, child)

	err := runner.handleOneShotCompletion(
		t.Context(),
		nil,
		nil,
		nil,
		&syncdomain.ActionCompletion{
			ActionID:   1,
			Path:       "root.txt",
			ActionType: syncdomain.ActionUpload,
			Err:        syncdomain.ErrActionPreconditionChanged,
			ErrMsg:     "source changed",
		},
	)

	require.NoError(t, err)
	assert.Empty(t, runner.outbox)
	assert.Equal(t, 0, runner.sched.graph.InFlightCount())
	assert.Equal(t, 0, runner.sched.runningCount)
	assert.Empty(t, listRetryWorkForTest(t, eng.baseline, t.Context()))

	succeeded, failed, errs := runner.resultStats()
	assert.Equal(t, 0, succeeded)
	assert.Equal(t, 0, failed)
	assert.Empty(t, errs)
}

// Validates: R-2.10.5
func TestEngineFlow_CompleteQueuedDispatchAsShutdown_CompletesQueuedSubtree(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	runner := newOneShotRunner(eng.Engine)
	runner.sched.graph = syncexec.NewDepGraph(eng.logger)
	runner.sched.dispatchCh = make(chan *syncdomain.TrackedAction, 1)

	root := runner.sched.graph.Add(&syncdomain.Action{
		Type: syncdomain.ActionUpload,
		Path: "root.txt",
	}, 1, nil)
	require.NotNil(t, root)

	child := runner.sched.graph.Add(&syncdomain.Action{
		Type: syncdomain.ActionUpload,
		Path: "child.txt",
		View: &syncdomain.PathView{Path: "child.txt"},
	}, 2, []int64{1})
	assert.Nil(t, child)

	runner.sched.dispatchCh <- root
	runner.completeQueuedDispatchAsShutdown()

	assert.Equal(t, 0, runner.sched.graph.InFlightCount())
}

// Validates: R-2.10.5
func TestEngineFlow_CompleteOutboxAsShutdown_CompletesTrackedActions(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	flow := testEngineFlow(t, eng)
	flow.sched.graph = syncexec.NewDepGraph(eng.logger)

	root := flow.sched.graph.Add(&syncdomain.Action{
		Type: syncdomain.ActionUpload,
		Path: "root.txt",
	}, 1, nil)
	require.NotNil(t, root)

	child := flow.sched.graph.Add(&syncdomain.Action{
		Type: syncdomain.ActionUpload,
		Path: "child.txt",
	}, 2, []int64{1})
	assert.Nil(t, child)

	flow.completeOutboxAsShutdown([]*syncdomain.TrackedAction{root})

	assert.Equal(t, 0, flow.sched.graph.InFlightCount())
}

// Validates: R-2.10.5, R-6.8
func TestOneShotRunner_HandleOneShotCompletion_AfterFatalCompletesReleasedReadyAsShutdown(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	runner := newOneShotRunner(eng.Engine)
	runner.sched.graph = syncexec.NewDepGraph(eng.logger)
	runner.sched.dispatchCh = make(chan *syncdomain.TrackedAction, 1)

	root := runner.sched.graph.Add(&syncdomain.Action{
		Type: syncdomain.ActionUpload,
		Path: "root.txt",
		View: &syncdomain.PathView{Path: "root.txt"},
	}, 1, nil)
	require.NotNil(t, root)

	child := runner.sched.graph.Add(&syncdomain.Action{
		Type: syncdomain.ActionUpload,
		Path: "child.txt",
		View: &syncdomain.PathView{Path: "child.txt"},
	}, 2, []int64{1})
	assert.Nil(t, child)

	err := runner.handleOneShotCompletion(
		t.Context(),
		nil,
		nil,
		assert.AnError,
		&syncdomain.ActionCompletion{
			ActionID:   1,
			Path:       "root.txt",
			ActionType: syncdomain.ActionUpload,
			Success:    true,
		},
	)

	require.ErrorIs(t, err, assert.AnError)
	assert.Empty(t, runner.outbox, "shutdown completion should consume newly-ready dependents immediately")
	assert.Equal(t, 0, runner.sched.graph.InFlightCount(), "released dependents should complete as shutdown work, not remain dispatchable")
}

// Validates: R-2.10.5, R-6.8
func TestOneShotRunner_HandleOneShotCompletion_AfterCancelCompletesReleasedReadyAsShutdown(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	runner := newOneShotRunner(eng.Engine)
	runner.sched.graph = syncexec.NewDepGraph(eng.logger)
	runner.sched.dispatchCh = make(chan *syncdomain.TrackedAction, 1)

	root := runner.sched.graph.Add(&syncdomain.Action{
		Type: syncdomain.ActionUpload,
		Path: "root.txt",
	}, 1, nil)
	require.NotNil(t, root)

	child := runner.sched.graph.Add(&syncdomain.Action{
		Type: syncdomain.ActionUpload,
		Path: "child.txt",
	}, 2, []int64{1})
	assert.Nil(t, child)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := runner.handleOneShotCompletion(
		ctx,
		nil,
		nil,
		nil,
		&syncdomain.ActionCompletion{
			ActionID:   1,
			Path:       "root.txt",
			ActionType: syncdomain.ActionUpload,
			Success:    true,
		},
	)

	require.NoError(t, err)
	assert.Empty(t, runner.outbox, "canceled shutdown completion should consume newly-ready dependents immediately")
	assert.Equal(t, 0, runner.sched.graph.InFlightCount(), "released dependents should complete as shutdown work after cancellation too")
}

// Validates: R-2.10.33
func TestOneShotRunner_RunResultsLoopIdle_ReleasesDueHeldWorkBeforeBlocking(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	runner := newOneShotRunner(eng.Engine)
	runner.sched.graph = syncexec.NewDepGraph(eng.logger)
	runner.sched.dispatchCh = make(chan *syncdomain.TrackedAction, 1)

	action := runner.sched.graph.Add(&syncdomain.Action{
		Type: syncdomain.ActionUpload,
		Path: "retry.txt",
		View: &syncdomain.PathView{Path: "retry.txt"},
	}, 1, nil)
	require.NotNil(t, action)
	runner.holdAction(action, heldReasonRetry, syncdomain.ScopeKey{}, eng.nowFunc().Add(-time.Second))

	ctx, cancel := context.WithCancel(t.Context())
	results := make(chan syncdomain.ActionCompletion, 1)

	syncfixture.RunInBackground(t, ctx, func(workerCtx context.Context) error {
		defer close(results)
		select {
		case dispatched := <-runner.sched.dispatchCh:
			results <- syncdomain.ActionCompletion{
				ActionID:   dispatched.ID,
				Path:       dispatched.Action.Path,
				ActionType: dispatched.Action.Type,
				Success:    true,
			}
			cancel()
		case <-workerCtx.Done():
		}
		return nil
	})

	err := syncfixture.RunWithin(t, ctx, "the one-shot results loop to release due held work while idle", func(loopCtx context.Context) error {
		return runner.runResultsLoopWithInitialOutbox(loopCtx, nil, nil, results, nil)
	})
	require.NoError(t, err)

	assert.Equal(t, 0, runner.sched.graph.InFlightCount())
	assert.Empty(t, runner.retries.heldByKey)
}

// Validates: R-2.10.33
func TestOneShotRunner_ReleaseIdleDueHeldWork_ClearsShutdownCompletedOutboxOnReductionFailure(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	runner := newOneShotRunner(eng.Engine)
	runner.sched.graph = syncexec.NewDepGraph(eng.logger)
	runner.sched.dispatchCh = make(chan *syncdomain.TrackedAction, 1)
	now := eng.nowFunc()

	concrete := runner.sched.graph.Add(&syncdomain.Action{
		Type: syncdomain.ActionUpload,
		Path: "retry.txt",
		View: &syncdomain.PathView{Path: "retry.txt"},
	}, 1, nil)
	require.NotNil(t, concrete)
	runner.holdAction(concrete, heldReasonRetry, syncdomain.ScopeKey{}, now.Add(-time.Second))

	publication := runner.sched.graph.Add(&syncdomain.Action{
		Type:    syncdomain.ActionCleanup,
		Path:    "cleanup.txt",
		DriveID: eng.driveID,
		ItemID:  "cleanup-item",
	}, 2, nil)
	require.NotNil(t, publication)
	runner.holdAction(publication, heldReasonRetry, syncdomain.ScopeKey{}, now.Add(-time.Second))

	require.NoError(t, eng.baseline.Close(t.Context()))

	err, handled := runner.releaseIdleDueHeldWork(t.Context(), &syncdomain.Baseline{})
	require.True(t, handled)
	require.ErrorContains(t, err, "reading observation state for action freshness")
	assert.Empty(t, runner.outbox, "shutdown-completed releases must not be queued for a second completion pass")
	assert.Equal(t, 1, runner.sched.graph.InFlightCount(), "only the failing publication action should remain unresolved")

	_, concretePresent := runner.sched.graph.Get(1)
	assert.False(t, concretePresent)
	_, publicationPresent := runner.sched.graph.Get(2)
	assert.True(t, publicationPresent)
}
