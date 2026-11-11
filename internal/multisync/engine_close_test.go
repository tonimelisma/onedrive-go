package multisync

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

// A one-shot engine whose close runs over is closed under the same bound as a
// watch runner's. The pass's report stands, the result carries ErrSyncStuck
// so the command exits 1, the drive is logged once with every goroutine's
// stack, and the close ran on a context detached from the run's cancellation
// but bounded. Before the bound the pass waited for the close with no limit,
// on the run's possibly canceled context.
//
// Validates: R-6.3.9
func TestRunOnce_AnEngineThatDoesNotCloseFailsTheRun(t *testing.T) {
	rd := testStandaloneMount(t, "personal:close-stuck@example.com", "CloseStuck")
	capture, logger := newLogCapture()
	cfg := testOrchestratorConfig(t, rd)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn
	cfg.Logger = logger
	orch := NewOrchestrator(cfg)

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	closeCtxs := make(chan context.Context, 1)
	orch.engineFactory = func(context.Context, engineFactoryRequest) (engineRunner, error) {
		return &mockEngine{
			report: &syncengine.Report{},
			closeFn: func(ctx context.Context) error {
				closeCtxs <- ctx
				<-release
				return nil
			},
		}, nil
	}
	var armed []time.Duration
	orch.retireTimer = func(bound time.Duration) (<-chan time.Time, func()) {
		armed = append(armed, bound)
		return firedTimer()
	}

	result := orch.RunOnce(t.Context(), syncengine.SyncBidirectional, syncengine.RunOptions{})

	require.ErrorIs(t, result.Stuck, ErrSyncStuck)
	require.ErrorContains(t, result.Stuck, "CloseStuck")
	require.Len(t, result.Reports, 1)
	require.NoError(t, result.Reports[0].Err, "the pass itself succeeded")
	assert.Equal(t, []time.Duration{engineCloseTimeout}, armed)
	errorLogs := capture.atLeast(slog.LevelError)
	require.Len(t, errorLogs, 1)
	assert.Contains(t, errorLogs[0].attrs["stack"], "goroutine ")
	closeCtx := <-closeCtxs
	require.NoError(t, closeCtx.Err())
	deadline, hasDeadline := closeCtx.Deadline()
	require.True(t, hasDeadline, "the close is bounded")
	assert.WithinDuration(t, time.Now().Add(engineCloseTimeout), deadline, engineCloseTimeout)
}

// A shortcut whose engine did not close after a one-shot pass may still be
// writing its store, so the pass neither acknowledges its final drain nor
// cleans up its state, and the run fails with ErrSyncStuck. The drive's own
// engine, which closed, is not reported stuck.
//
// Validates: R-6.3.9, R-2.4.8
func TestRunOnce_AShortcutEngineThatDoesNotCloseIsNotAcknowledged(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	parent := testStandaloneMount(t, "personal:child-close-stuck@example.com", "Parent")
	child := testChildRecord(t, mountID(parent.CanonicalID.String()), "binding-close-stuck", "Shortcuts/Stuck")
	child.Mode = syncengine.ShortcutChildRunModeFinalDrain
	capture, logger := newLogCapture()
	cfg := testOrchestratorConfig(t, parent)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn
	cfg.Logger = logger
	orch := NewOrchestrator(cfg)

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	var acks atomic.Int32
	orch.engineFactory = func(_ context.Context, req engineFactoryRequest) (engineRunner, error) {
		if req.Mount.projectionKind() == MountProjectionChild {
			return &mockEngine{
				report: &syncengine.Report{},
				closeFn: func(context.Context) error {
					<-release
					return nil
				},
			}, nil
		}
		return &mockEngine{
			runOnceFn: func(ctx context.Context, _ syncengine.SyncMode, _ syncengine.RunOptions) (*syncengine.Report, error) {
				if err := req.Mount.shortcutChildWorkSink()(ctx, processSnapshot(parent.CanonicalID.String(), child)); err != nil {
					return nil, err
				}
				return &syncengine.Report{}, nil
			},
			ackDrainFn: func(context.Context, syncengine.ShortcutChildDrainAck) (syncengine.ShortcutChildWorkSnapshot, error) {
				acks.Add(1)
				return syncengine.ShortcutChildWorkSnapshot{}, nil
			},
		}, nil
	}
	// The shortcut's close is the first one waited for and never finishes;
	// the drive's close comes after the pass and finishes at once.
	arms := 0
	never := make(chan time.Time)
	orch.retireTimer = func(time.Duration) (<-chan time.Time, func()) {
		arms++
		if arms == 1 {
			return firedTimer()
		}
		return never, func() {}
	}

	result := orch.RunOnce(t.Context(), syncengine.SyncBidirectional, syncengine.RunOptions{})

	require.ErrorIs(t, result.Stuck, ErrSyncStuck)
	require.ErrorContains(t, result.Stuck, "Stuck")
	assert.NotContains(t, result.Stuck.Error(), "Parent:", "the drive's own engine closed")
	assert.Zero(t, acks.Load(), "a shortcut whose engine may still write its store is not acknowledged")
	assert.Len(t, capture.atLeast(slog.LevelError), 1)
}
