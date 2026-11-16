package sync

import (
	"context"
	"testing"
	gosynctest "testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// waitNotBoundedByEventWait is a child's failure when a wait did not give up
// exactly eventWait after it began.
const waitNotBoundedByEventWait = "the wait did not give up exactly eventWait after it began"

// assertGaveUpAfterEventWait checks, on the bubble's clock, that the wait that
// began at start gave up after eventWait.
func assertGaveUpAfterEventWait(t *testing.T, start time.Time) {
	t.Helper()

	assert.Equal(t, eventWait, time.Since(start), waitNotBoundedByEventWait)
}

// runIgnoringCancellation returns a run that ignores its context and returns
// only when the test's cleanups release it. Its cleanup is registered before
// the helper under test registers its own, so it runs after that one.
func runIgnoringCancellation(t *testing.T) func(context.Context) error {
	t.Helper()

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	return func(context.Context) error {
		<-release
		return nil
	}
}

// requireChildFailedWith requires that the child test binary ran the test and
// failed it with want in its output, at the bound.
func requireChildFailedWith(t *testing.T, result synctest.ChildResult, want string) {
	t.Helper()

	require.Equal(t, 1, result.ExitCode, "the child test binary did not fail its test:\n%s", result.Output)
	require.Contains(t, result.Output, "--- FAIL: "+t.Name()+" ", "the child test binary did not run the test:\n%s", result.Output)
	assert.Contains(t, result.Output, want, "the failure does not name what was waited for")
	assert.NotContains(t, result.Output, waitNotBoundedByEventWait)
}

// Each wait helper fails its test naming what it waited for when the event
// does not come within eventWait. The wait runs in a child test binary inside
// a testing/synctest bubble, whose clock passes eventWait at once, and the
// parent reads the child's failure.
//
// Validates: R-6.10.10
func TestWatchHelpers_FailNamingWhatTheyWaitedFor(t *testing.T) {
	tests := []struct {
		name string
		wait func(t *testing.T)
		want string
	}{
		{
			name: "receiveWithin",
			wait: func(t *testing.T) {
				t.Helper()
				defer assertGaveUpAfterEventWait(t, time.Now())
				receiveWithin(t, make(chan struct{}), "the event under test")
			},
			want: "timed out waiting for the event under test",
		},
		{
			name: "runWithin",
			wait: func(t *testing.T) {
				t.Helper()
				defer assertGaveUpAfterEventWait(t, time.Now())
				require.NoError(t, runWithin(t, t.Context(), "the run under test to return", runIgnoringCancellation(t)))
			},
			want: "timed out waiting for the run under test to return",
		},
		{
			name: "stopWatch",
			wait: func(t *testing.T) {
				t.Helper()
				defer assertGaveUpAfterEventWait(t, time.Now())
				cancel, done := runInBackground(t, t.Context(), runIgnoringCancellation(t))
				stopWatch(t, cancel, done)
			},
			want: "timed out waiting for the watch to return after cancellation",
		},
		{
			// The wait is the cleanup's, after the test body has returned.
			name: "runInBackground",
			wait: func(t *testing.T) {
				t.Helper()
				start := time.Now()
				t.Cleanup(func() { assertGaveUpAfterEventWait(t, start) })
				runInBackground(t, t.Context(), runIgnoringCancellation(t))
			},
			want: "a background watch did not return after its cleanup canceled it",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !synctest.InChild(t) {
				requireChildFailedWith(t, synctest.RunInChild(t), tt.want)
				return
			}

			gosynctest.Test(t, tt.wait)
		})
	}
}
