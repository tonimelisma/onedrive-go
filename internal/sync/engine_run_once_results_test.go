package sync

import (
	"context"
	"net/http"
	"testing"

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

	err := runner.runResultsLoop(t.Context(), nil, nil, results)
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

	err := runner.runResultsLoop(t.Context(), nil, nil, results)
	require.ErrorIs(t, err, graph.ErrUnauthorized)
	assert.Equal(t, 0, runner.depGraph.InFlightCount())
	assert.Empty(t, listRetryWorkForTest(t, eng.baseline, t.Context()))
	assert.Empty(t, actionableObservationIssuesForTest(t, eng.baseline, t.Context()))

	required, authErr := eng.hasPersistedAccountAuthRequirement()
	require.NoError(t, authErr)
	assert.True(t, required)
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
