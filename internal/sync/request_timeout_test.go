package sync

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncexec"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncshortcut"
)

// stalledGraphRequestError is the error graph returns for one item request
// to a stalled server through kind's transport.
func stalledGraphRequestError(t *testing.T, kind stalledRequestKind) error {
	t.Helper()

	server, reached := syncfixture.NewStalledServer(t)
	client, dispatched := syncfixture.StalledGraphClient(t, server, kind.client(t))

	_, err := client.GetItem(t.Context(), driveid.New(syncfixture.EngineTestDriveID), "stalled-item")
	require.Error(t, err)
	assert.Equal(t, int64(1), dispatched.Load(), "one dispatch is one request")
	if !kind.reachesServer {
		assert.Zero(t, reached.Load(), "the dial gave up before a connection existed")
	}

	return fmt.Errorf("get the stalled item: %w", err)
}

// Every transport timeout reaches sync carrying context.DeadlineExceeded
// while the run goes on: it is a request that timed out, kept as retry work
// with its issue, not a shutdown. Once the run's context has ended the same
// error is the shutdown's own.
//
// Validates: R-6.8.29, R-6.8.10
func TestClassifyResult_TransportTimeoutIsARequestTimeoutUntilTheRunEnds(t *testing.T) {
	t.Parallel()

	for _, kind := range stalledRequestKinds() {
		t.Run(kind.name, func(t *testing.T) {
			t.Parallel()

			err := stalledGraphRequestError(t, kind)
			require.ErrorIs(t, err, context.DeadlineExceeded, "the transport's timeout has the shape of a deadline")

			live := classifyResult(&syncdomain.ActionCompletion{Err: err, HTTPStatus: syncdomain.ExtractHTTPStatus(err)})
			assert.Equal(t, resultRequeue, live.Class, "%v", err)
			assert.Equal(t, syncdomain.PersistRetryWork, live.Persistence)
			assert.Equal(t, syncdomain.IssueRequestTimeout, live.ConditionType)

			ended := classifyResult(&syncdomain.ActionCompletion{Err: err, HTTPStatus: syncdomain.ExtractHTTPStatus(err), RunEnded: true})
			assert.Equal(t, resultShutdown, ended.Class)
		})
	}
}

// Whether work failed because its run is ending is a fact about the run's
// context, never about the error: a deadline-shaped error with a live run is
// not a stop, any failure once the run has ended is, and no error is never.
//
// Validates: R-6.8.29
func TestRunEnded_AsksTheRunNotTheError(t *testing.T) {
	t.Parallel()

	ended, cancel := context.WithCancel(t.Context())
	cancel()
	failure := errors.New("request failed")

	assert.False(t, syncdomain.RunEnded(t.Context(), context.DeadlineExceeded), "a deadline while the run goes on")
	assert.False(t, syncdomain.RunEnded(t.Context(), context.Canceled), "a cancel narrower than the run")
	assert.True(t, syncdomain.RunEnded(ended, failure), "any failure once the run has ended")
	assert.False(t, syncdomain.RunEnded(ended, nil), "no error is never a stop")
}

// A failure to hand a poll's batch over, from the batch handler or from the
// batch's apply, ends the remote watch with that failure while the run goes
// on, even when it has the shape of a deadline. The same failure once the run
// has ended is the watch's stop.
//
// Validates: R-6.8.29
func TestRemoteObserverWatch_AFailedBatchEndsTheWatchUnlessItsRunEnded(t *testing.T) {
	t.Parallel()

	// A store write that ran out of time.
	failure := fmt.Errorf("commit observations: %w", context.DeadlineExceeded)
	for _, tc := range []struct {
		name      string
		failApply bool
		endRun    bool
	}{
		{name: "handler fails while the run goes on"},
		{name: "handler fails once the run ended", endRun: true},
		{name: "apply fails while the run goes on", failApply: true},
		{name: "apply fails once the run ended", failApply: true, endRun: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fetcher := &syncfixture.ScriptedDeltaFetcher{Answer: func(int, string) (*graph.DeltaPage, error) {
				return &graph.DeltaPage{Items: []graph.Item{syncfixture.CursorTestItem("a")}, DeltaLink: "token-after"}, nil
			}}
			obs := syncobserve.NewRemoteObserver(fetcher, syncfixture.EmptyBaseline(), driveid.New(syncfixture.EngineTestDriveID), syncfixture.TestLogger(t))
			runCtx, endRun := context.WithCancel(t.Context())
			defer endRun()
			handle := func(context.Context, []syncdomain.RemoteItem, string, syncshortcut.ShortcutTopologyBatch) (syncobserve.RemoteObservationBatch, error) {
				if tc.endRun {
					endRun()
				}
				if !tc.failApply {
					return syncobserve.RemoteObservationBatch{}, failure
				}
				applied := make(chan error, 1)
				applied <- failure

				return syncobserve.RemoteObservationBatch{ApplyAck: applied}, nil
			}

			err := syncfixture.RunWithin(t, runCtx, "the remote watch to return", func(ctx context.Context) error {
				return obs.Watch(ctx, "saved-token", make(chan syncobserve.RemoteObservationBatch, 1), syncdomain.MinPollInterval, nil, handle)
			})

			if tc.endRun {
				require.NoError(t, err, "a failure once the run has ended is the stop")
				return
			}
			require.ErrorIs(t, err, failure)
		})
	}
}

// Each cancel the worker pool and the one-shot pass make carries a cause of
// its own, and the cause answers to context.Canceled: a request it interrupts
// carries the cause in place of context.Canceled and still reads as canceled.
//
// Validates: R-6.8.29
func TestCancelCauses_AnswerToCanceledAndNameTheirCancel(t *testing.T) {
	t.Parallel()

	texts := make(map[string]string)
	for _, tc := range []struct {
		name  string
		cause error
	}{
		{name: "action returned", cause: syncexec.ErrActionReturned},
		{name: "worker pool stopped", cause: syncexec.ErrWorkerPoolStopped},
		{name: "one-shot pass failed", cause: errOneShotPassFailed},
		{name: "one-shot pass ended", cause: errOneShotPassEnded},
	} {
		require.ErrorIs(t, tc.cause, context.Canceled, tc.name)
		require.NotErrorIs(t, tc.cause, context.DeadlineExceeded, tc.name)
		assert.True(t, isCancellationEcho(fmt.Errorf("request canceled: %w", tc.cause)), tc.name)
		assert.NotContains(t, texts, tc.cause.Error(), "%s has a text of its own", tc.name)
		texts[tc.cause.Error()] = tc.name
	}
}

// A watch step that failed is the stop only when the run has ended and the
// failure is that end's own cancellation coming back: a disk error during the
// stop, or a store that did not answer within its bound, is still the watch's
// error, and while the run goes on neither is a stop.
//
// Validates: R-6.8.29, R-2.8.3
func TestIsWatchShutdownError_OnlyTheEndedRunsOwnCancellation(t *testing.T) {
	t.Parallel()

	ended, cancel := context.WithCancel(t.Context())
	cancel()
	canceled := fmt.Errorf("watch step: %w", context.Canceled)
	expired := fmt.Errorf("watch step: %w", context.DeadlineExceeded)
	diskFailure := errors.New("write sync state: disk I/O error")
	storeSilent := fmt.Errorf("record retry work: %w: %w", context.DeadlineExceeded, syncdomain.ErrStoreAccessTimedOut)

	assert.True(t, isWatchShutdownError(ended, canceled), "the cancel coming back")
	assert.True(t, isWatchShutdownError(ended, expired), "an ended context's deadline coming back")
	assert.False(t, isWatchShutdownError(ended, diskFailure), "a separate failure during the stop")
	assert.False(t, isWatchShutdownError(ended, storeSilent), "a store that did not answer within its bound")
	assert.False(t, isWatchShutdownError(t.Context(), canceled), "a cancel while the run goes on")
	assert.False(t, isWatchShutdownError(t.Context(), expired), "a timed-out request while the run goes on")
	assert.False(t, isWatchShutdownError(t.Context(), diskFailure), "a failure while the run goes on")
}
