package sync

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/panicguard"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// panicATimerAndWaitForTheCancel is a download that, once in flight, starts a
// timer on the engine's real clock whose callback panics, then waits for its
// run to be canceled and fails as the transports fail a canceled request,
// with the context's cause.
func panicATimerAndWaitForTheCancel(eng *Engine) func(context.Context, driveid.ID, string, io.Writer) (int64, error) {
	return func(ctx context.Context, _ driveid.ID, _ string, _ io.Writer) (int64, error) {
		eng.wallClock.AfterFunc(0, func() { panic("timer exploded") })
		<-ctx.Done()

		return 0, fmt.Errorf("request canceled: %w", context.Cause(ctx))
	}
}

// requireRunEndedByTheTimerPanic checks a run's error against the timer
// panic that ended it: the panic's error alone, the one the boundary logged
// once, with the stack of the callback that panicked.
func requireRunEndedByTheTimerPanic(t *testing.T, capture *synctest.LogCapture, err error) {
	t.Helper()

	require.ErrorIs(t, err, panicguard.ErrPanic)
	require.NotErrorIs(t, err, context.Canceled, "the cancel the panic caused is not reported beside it")

	var logged []synctest.CapturedRecord
	for _, record := range capture.AtLeast(slog.LevelError) {
		if strings.HasPrefix(record.Message, "Internal error ") {
			logged = append(logged, record)
		}
	}
	require.Len(t, logged, 1, "the panic is logged once")
	assert.Equal(t, "timer exploded", logged[0].Attrs["panic"])
	assert.Contains(t, logged[0].Attrs["stack"], "panicATimerAndWaitForTheCancel")
	assert.Equal(t, logged[0].Attrs["stack"], panicguard.Stack(err), "the run returns the error the boundary logged")
}

// assertNoFailureRecorded checks that the action the panic's cancel stopped
// was read as a shutdown: no retry work, no blocked scope.
func assertNoFailureRecorded(t *testing.T, eng *Engine) {
	t.Helper()

	retry, err := eng.baseline.ListRetryWork(t.Context())
	require.NoError(t, err)
	assert.Empty(t, retry, "a shutdown leaves no retry work")
	scopes, err := eng.baseline.ListBlockScopes(t.Context())
	require.NoError(t, err)
	assert.Empty(t, scopes, "a shutdown blocks nothing")
}

// Validates: R-6.3.10, R-6.3.8
//
// A timer callback runs on a goroutine of its own, outside every run's call
// stack. When one panics during a one-shot sync, the real clock's boundary
// logs it once with its stack and cancels the run plainly, and RunOnce
// returns an error wrapping panicguard.ErrPanic beside its report. The
// download in flight ends as at a stop: no failure and no retry work is
// recorded, and the next sync downloads the file.
func TestRunOnce_ATimerPanicEndsTheRunAndTheActionInFlightIsAShutdown(t *testing.T) {
	t.Parallel()

	if !synctest.InChild(t) {
		synctest.RequireChildPassed(t, synctest.RunInChild(t))
		return
	}

	capture, logger := synctest.NewLogCapture()
	mock := newRunOnceBidirectionalMock(driveid.New(engineTestDriveID))
	downloadContent := mock.downloadFn
	eng, syncRoot := newTestEngineWithLogger(t, mock, logger)
	mock.downloadFn = panicATimerAndWaitForTheCancel(eng.Engine)

	report, err := eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})

	requireRunEndedByTheTimerPanic(t, capture, err)
	require.NotNil(t, report, "RunOnce returns the report it has beside the error")
	assert.Zero(t, report.Failed)
	assert.Zero(t, report.Succeeded)
	assertNoFailureRecorded(t, eng.Engine)

	mock.downloadFn = downloadContent
	report, err = eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
	require.NoError(t, err, "a panic ends only the run it fired in")
	assert.Equal(t, 1, report.Succeeded, "the next sync redoes the action the panic stopped")
	assert.FileExists(t, filepath.Join(syncRoot, "remote.txt"))
}

// Validates: R-6.3.10, R-6.3.8
//
// The same for a watch: a timer callback's panic ends RunWatch with an error
// wrapping panicguard.ErrPanic, which multisync answers by restarting the
// drive on its backoff, and the download in flight is a shutdown.
func TestRunWatch_ATimerPanicEndsTheRunAndTheActionInFlightIsAShutdown(t *testing.T) {
	t.Parallel()

	if !synctest.InChild(t) {
		synctest.RequireChildPassed(t, synctest.RunInChild(t))
		return
	}

	capture, logger := synctest.NewLogCapture()
	mock := newRunOnceBidirectionalMock(driveid.New(engineTestDriveID))
	eng, _ := newTestEngineWithLogger(t, mock, logger)
	mock.downloadFn = panicATimerAndWaitForTheCancel(eng.Engine)

	done := make(chan error, 1)
	go func() {
		done <- eng.RunWatch(t.Context(), SyncBidirectional, WatchOptions{PollInterval: time.Hour, Debounce: 5 * time.Millisecond})
	}()

	select {
	case err := <-done:
		requireRunEndedByTheTimerPanic(t, capture, err)
	case <-time.After(30 * time.Second):
		require.FailNow(t, "the timer's panic did not end the watch")
	}
	assertNoFailureRecorded(t, eng.Engine)
}

// panicThroughTheBoundary panics inside clock's timer boundary, as a timer
// callback the clock runs would.
func panicThroughTheBoundary(clock realClock, value string) {
	defer clock.recoverTimerPanic()

	panic(value)
}

// Validates: R-6.3.10
//
// A timer callback that panics while no run is bound, after its run returned
// or on a clock no engine bound, leaves its log line and nothing else: the
// next run is neither canceled nor failed by it.
func TestRealClock_ATimerPanicWithNoRunBoundIsOnlyLogged(t *testing.T) {
	t.Parallel()

	capture, logger := synctest.NewLogCapture()
	eng := &Engine{wallClock: realClock{logger: logger, panics: &timerPanics{}}}
	bound := eng.wallClock
	_, end := eng.beginRun(t.Context())
	require.NoError(t, end(nil))

	require.NotPanics(t, func() { panicThroughTheBoundary(bound, "late timer exploded") }, "after its run returned")
	require.NotPanics(t, func() { panicThroughTheBoundary(realClock{logger: logger}, "unbound timer exploded") },
		"on a clock no engine bound")

	logged := capture.AtLeast(slog.LevelError)
	require.Len(t, logged, 2, "each panic is logged")
	assert.Equal(t, "late timer exploded", logged[0].Attrs["panic"])
	assert.Equal(t, "unbound timer exploded", logged[1].Attrs["panic"])

	next, endNext := eng.beginRun(t.Context())
	require.NoError(t, next.Err(), "a panic that fired between runs cancels no later run")
	require.NoError(t, endNext(nil), "nor fails it")
}

// Validates: R-6.3.10
//
// A timer callback's panic ends the run only when the callback ran on the
// clock that reports to the engine's timer panics. So the engine builds that
// one real clock, in newEngine, and every observer it builds runs on it: the
// observer constructors are called only from the two engine methods that set
// the clock. The constructors' own zero clock is for tests that build an
// observer by hand.
func TestEngineBuildsOneRealClockAndEveryObserverRunsOnIt(t *testing.T) {
	t.Parallel()

	paths, err := filepath.Glob("*.go")
	require.NoError(t, err)

	fset := token.NewFileSet()
	var clocks, constructorCalls []string
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		require.NoError(t, err)
		for _, decl := range file.Decls {
			owner := "package scope"
			if fn, ok := decl.(*ast.FuncDecl); ok {
				owner = fn.Name.Name
			}
			ast.Inspect(decl, func(node ast.Node) bool {
				switch n := node.(type) {
				case *ast.CompositeLit:
					if ident, ok := n.Type.(*ast.Ident); ok && ident.Name == "realClock" && len(n.Elts) > 0 {
						clocks = append(clocks, owner)
					}
				case *ast.CallExpr:
					if ident, ok := n.Fun.(*ast.Ident); ok && (ident.Name == "newLocalObserver" || ident.Name == "newRemoteObserver") {
						constructorCalls = append(constructorCalls, ident.Name+" from "+owner)
					}
				}
				return true
			})
		}
	}

	assert.Equal(t, []string{"newEngine"}, clocks, "one real clock with the engine's timer panics, built in newEngine")
	assert.ElementsMatch(t,
		[]string{"newLocalObserver from buildLocalObserver", "newRemoteObserver from buildRemoteObserver"},
		constructorCalls, "every observer the engine builds gets the engine's real clock")
}
