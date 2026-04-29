package sync

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// Validates: R-6.8.9
func TestOneShotEngineLoop_ClosedResultsStillProcessBufferedRetryWork(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	runner := newOneShotRunner(eng.Engine)
	runner.depGraph = NewDepGraph(eng.logger)
	runner.dispatchCh = make(chan *TrackedAction, 16)

	for _, actionID := range []int64{1, 2, 3} {
		runner.depGraph.Add(&Action{
			Path: "action.txt",
			Type: ActionUpload,
		}, actionID, nil)
	}

	results := make(chan ActionCompletion, 3)
	results <- ActionCompletion{
		ActionID:   1,
		Path:       "a.txt",
		ActionType: ActionUpload,
		HTTPStatus: http.StatusBadGateway,
		Err:        graph.ErrServerError,
		ErrMsg:     "fail-1",
	}
	results <- ActionCompletion{
		ActionID:   2,
		Path:       "b.txt",
		ActionType: ActionUpload,
		HTTPStatus: http.StatusBadGateway,
		Err:        graph.ErrServerError,
		ErrMsg:     "fail-2",
	}
	results <- ActionCompletion{
		ActionID:   3,
		Path:       "c.txt",
		ActionType: ActionDownload,
		Success:    true,
	}
	close(results)

	err := runner.runResultsLoopWithInitialOutbox(t.Context(), nil, nil, results, nil)
	require.NoError(t, err)

	retryRows := listRetryWorkForTest(t, eng.baseline, t.Context())
	require.Len(t, retryRows, 2)
	assert.ElementsMatch(t, []string{"a.txt", "b.txt"}, []string{retryRows[0].Path, retryRows[1].Path})
}

// Validates: R-2.10.5
func TestOneShotEngineLoop_UnauthorizedTerminatesAndDrainsQueuedReady(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	runner := newOneShotRunner(eng.Engine)
	runner.depGraph = NewDepGraph(eng.logger)
	runner.dispatchCh = make(chan *TrackedAction)

	runner.depGraph.Add(&Action{
		Type: ActionUpload,
		Path: "root.txt",
	}, 1, nil)
	runner.depGraph.Add(&Action{
		Type: ActionUpload,
		Path: "child.txt",
	}, 2, []int64{1})
	runner.depGraph.Add(&Action{
		Type: ActionDownload,
		Path: "auth.txt",
	}, 3, nil)

	results := make(chan ActionCompletion, 2)
	results <- ActionCompletion{
		ActionID:   1,
		Path:       "root.txt",
		ActionType: ActionUpload,
		Success:    true,
	}
	results <- ActionCompletion{
		ActionID:   3,
		Path:       "auth.txt",
		ActionType: ActionDownload,
		HTTPStatus: http.StatusUnauthorized,
		Err:        graph.ErrUnauthorized,
		ErrMsg:     "unauthorized",
	}
	close(results)

	err := runner.runResultsLoopWithInitialOutbox(t.Context(), nil, nil, results, nil)
	require.ErrorIs(t, err, graph.ErrUnauthorized)
	assert.Equal(t, 0, runner.depGraph.InFlightCount())
	assert.Empty(t, listRetryWorkForTest(t, eng.baseline, t.Context()))
	assert.Empty(t, actionableObservationIssuesForTest(t, eng.baseline, t.Context()))

	required, authErr := eng.hasPersistedAccountAuthRequirement()
	require.NoError(t, authErr)
	assert.True(t, required)
}

// Validates: R-2.8.7, R-2.10.5
func TestOneShotEngineLoop_SupersededCompletionRetiresDependentsWithoutSuccessOrRetry(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	runner := newOneShotRunner(eng.Engine)
	runner.depGraph = NewDepGraph(eng.logger)
	runner.dispatchCh = make(chan *TrackedAction, 1)

	root := runner.depGraph.Add(&Action{
		Type: ActionUpload,
		Path: "root.txt",
	}, 1, nil)
	require.NotNil(t, root)
	runner.markRunning(root)

	child := runner.depGraph.Add(&Action{
		Type: ActionDownload,
		Path: "child.txt",
	}, 2, []int64{1})
	assert.Nil(t, child)

	outbox, err := runner.handleOneShotCompletion(
		t.Context(),
		nil,
		nil,
		nil,
		nil,
		&ActionCompletion{
			ActionID:   1,
			Path:       "root.txt",
			ActionType: ActionUpload,
			Err:        ErrActionPreconditionChanged,
			ErrMsg:     "source changed",
		},
	)

	require.NoError(t, err)
	assert.Empty(t, outbox)
	assert.Equal(t, 0, runner.depGraph.InFlightCount())
	assert.Equal(t, 0, runner.runningCount)
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
	runner.depGraph = NewDepGraph(eng.logger)
	runner.dispatchCh = make(chan *TrackedAction, 1)

	root := runner.depGraph.Add(&Action{
		Type: ActionUpload,
		Path: "root.txt",
	}, 1, nil)
	require.NotNil(t, root)

	child := runner.depGraph.Add(&Action{
		Type: ActionUpload,
		Path: "child.txt",
	}, 2, []int64{1})
	assert.Nil(t, child)

	runner.dispatchCh <- root
	runner.completeQueuedDispatchAsShutdown()

	assert.Equal(t, 0, runner.depGraph.InFlightCount())
}

// Validates: R-2.10.5
func TestEngineFlow_CompleteOutboxAsShutdown_CompletesTrackedActions(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	flow := testEngineFlow(t, eng)
	flow.depGraph = NewDepGraph(eng.logger)

	root := flow.depGraph.Add(&Action{
		Type: ActionUpload,
		Path: "root.txt",
	}, 1, nil)
	require.NotNil(t, root)

	child := flow.depGraph.Add(&Action{
		Type: ActionUpload,
		Path: "child.txt",
	}, 2, []int64{1})
	assert.Nil(t, child)

	flow.completeOutboxAsShutdown([]*TrackedAction{root})

	assert.Equal(t, 0, flow.depGraph.InFlightCount())
}

// Validates: R-2.10.5, R-6.8
func TestOneShotRunner_HandleOneShotCompletion_AfterFatalCompletesReleasedReadyAsShutdown(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	runner := newOneShotRunner(eng.Engine)
	runner.depGraph = NewDepGraph(eng.logger)
	runner.dispatchCh = make(chan *TrackedAction, 1)

	root := runner.depGraph.Add(&Action{
		Type: ActionUpload,
		Path: "root.txt",
	}, 1, nil)
	require.NotNil(t, root)

	child := runner.depGraph.Add(&Action{
		Type: ActionUpload,
		Path: "child.txt",
	}, 2, []int64{1})
	assert.Nil(t, child)

	outbox, err := runner.handleOneShotCompletion(
		t.Context(),
		nil,
		nil,
		nil,
		assert.AnError,
		&ActionCompletion{
			ActionID:   1,
			Path:       "root.txt",
			ActionType: ActionUpload,
			Success:    true,
		},
	)

	require.ErrorIs(t, err, assert.AnError)
	assert.Empty(t, outbox, "shutdown completion should consume newly-ready dependents immediately")
	assert.Equal(t, 0, runner.depGraph.InFlightCount(), "released dependents should complete as shutdown work, not remain dispatchable")
}

// Validates: R-2.10.5, R-6.8
func TestOneShotRunner_HandleOneShotCompletion_AfterCancelCompletesReleasedReadyAsShutdown(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	runner := newOneShotRunner(eng.Engine)
	runner.depGraph = NewDepGraph(eng.logger)
	runner.dispatchCh = make(chan *TrackedAction, 1)

	root := runner.depGraph.Add(&Action{
		Type: ActionUpload,
		Path: "root.txt",
	}, 1, nil)
	require.NotNil(t, root)

	child := runner.depGraph.Add(&Action{
		Type: ActionUpload,
		Path: "child.txt",
	}, 2, []int64{1})
	assert.Nil(t, child)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	outbox, err := runner.handleOneShotCompletion(
		ctx,
		nil,
		nil,
		nil,
		nil,
		&ActionCompletion{
			ActionID:   1,
			Path:       "root.txt",
			ActionType: ActionUpload,
			Success:    true,
		},
	)

	require.NoError(t, err)
	assert.Empty(t, outbox, "canceled shutdown completion should consume newly-ready dependents immediately")
	assert.Equal(t, 0, runner.depGraph.InFlightCount(), "released dependents should complete as shutdown work after cancellation too")
}

// Validates: R-2.10.33
func TestOneShotRunner_RunResultsLoopIdle_ReleasesDueHeldWorkBeforeBlocking(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	runner := newOneShotRunner(eng.Engine)
	runner.depGraph = NewDepGraph(eng.logger)
	runner.dispatchCh = make(chan *TrackedAction, 1)

	action := runner.depGraph.Add(&Action{
		Type: ActionUpload,
		Path: "retry.txt",
	}, 1, nil)
	require.NotNil(t, action)
	runner.holdAction(action, heldReasonRetry, ScopeKey{}, eng.nowFn().Add(-time.Second))

	ctx, cancel := context.WithCancel(t.Context())
	results := make(chan ActionCompletion, 1)
	done := make(chan error, 1)

	go func() {
		defer close(results)
		dispatched := <-runner.dispatchCh
		results <- ActionCompletion{
			ActionID:   dispatched.ID,
			Path:       dispatched.Action.Path,
			ActionType: dispatched.Action.Type,
			Success:    true,
		}
		cancel()
	}()

	go func() {
		done <- runner.runResultsLoopWithInitialOutbox(ctx, nil, nil, results, nil)
	}()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		require.FailNow(t, "one-shot results loop did not release due held work while idle")
	}

	assert.Equal(t, 0, runner.depGraph.InFlightCount())
	assert.Empty(t, runner.heldByKey)
}

// Validates: R-2.10.33
func TestOneShotRunner_ReleaseIdleDueHeldWork_ClearsShutdownCompletedOutboxOnReductionFailure(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	runner := newOneShotRunner(eng.Engine)
	runner.depGraph = NewDepGraph(eng.logger)
	runner.dispatchCh = make(chan *TrackedAction, 1)
	now := eng.nowFunc()

	concrete := runner.depGraph.Add(&Action{
		Type: ActionUpload,
		Path: "retry.txt",
	}, 1, nil)
	require.NotNil(t, concrete)
	runner.holdAction(concrete, heldReasonRetry, ScopeKey{}, now.Add(-time.Second))

	publication := runner.depGraph.Add(&Action{
		Type:    ActionCleanup,
		Path:    "cleanup.txt",
		DriveID: eng.driveID,
		ItemID:  "cleanup-item",
	}, 2, nil)
	require.NotNil(t, publication)
	runner.holdAction(publication, heldReasonRetry, ScopeKey{}, now.Add(-time.Second))

	require.NoError(t, eng.baseline.Close(t.Context()))

	outbox, err, handled := runner.releaseIdleDueHeldWork(t.Context(), &Baseline{})
	require.True(t, handled)
	require.ErrorContains(t, err, "record retry_work")
	assert.Empty(t, outbox, "shutdown-completed releases must not be returned for a second completion pass")
	assert.Equal(t, 1, runner.depGraph.InFlightCount(), "only the failing publication action should remain unresolved")

	_, concretePresent := runner.depGraph.Get(1)
	assert.False(t, concretePresent)
	_, publicationPresent := runner.depGraph.Get(2)
	assert.True(t, publicationPresent)
}
