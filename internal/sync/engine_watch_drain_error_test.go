package sync

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-2.11, R-6.10.10
//
// Each step begins by draining when the context is already done, so a
// cancellation landing between steps is handled. One landing inside a step is
// not: the handler's own I/O fails with the context error, the step returns
// it, and returning that error leaves through a path that never drained.
// RunWatch then classifies the cancellation as a clean stop and returns nil,
// so the shutdown looks complete while the retry and trial timers it was
// supposed to stop are still armed.
//
// That is the shape of LI-20260830-01, which failed as "run returned before
// debug event: shutdown started: run returned <nil>".
func TestDrainAfterCanceledStepError_CanceledStepFailureStartsTheDrain(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	stepErr := fmt.Errorf("committing local observation: %w", context.Canceled)

	require.True(t, rt.drainAfterCanceledStepError(ctx, &watchPipeline{runtime: rt}, stepErr))
	assert.True(t, rt.isDraining(), "the loop must leave through the drain, not the error")
}

// Validates: R-2.11
//
// A failure with a live context is a real failure and must be reported.
func TestDrainAfterCanceledStepError_LiveContextReportsTheError(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)

	assert.False(t, rt.drainAfterCanceledStepError(
		t.Context(), &watchPipeline{runtime: rt}, errors.New("disk failure")))
	assert.False(t, rt.isDraining())
}

// Validates: R-2.11
//
// A second failure while already draining is returned rather than converted,
// so the loop cannot spin on a step that keeps failing.
func TestDrainAfterCanceledStepError_AlreadyDrainingReportsTheError(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	pipe := &watchPipeline{runtime: rt}
	require.True(t, rt.drainAfterCanceledStepError(ctx, pipe, context.Canceled))
	assert.False(t, rt.drainAfterCanceledStepError(ctx, pipe, context.Canceled),
		"a repeat failure while draining must be reported instead of looping")
}

// Validates: R-2.11
func TestDrainAfterCanceledStepError_NoErrorDoesNothing(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	assert.False(t, rt.drainAfterCanceledStepError(ctx, &watchPipeline{runtime: rt}, nil))
	assert.False(t, rt.isDraining())
}
