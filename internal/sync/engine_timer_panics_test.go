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
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/panicguard"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
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
	mock := newRunOnceBidirectionalMock(driveid.New(syncfixture.EngineTestDriveID))
	downloadContent := mock.DownloadFn
	eng, syncRoot := newTestEngineWithLogger(t, mock, logger)
	mock.DownloadFn = panicATimerAndWaitForTheCancel(eng.Engine)

	report, err := eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{})

	requireRunEndedByTheTimerPanic(t, capture, err)
	require.NotNil(t, report, "RunOnce returns the report it has beside the error")
	assert.Zero(t, report.Failed)
	assert.Zero(t, report.Succeeded)
	assertNoFailureRecorded(t, eng.Engine)

	mock.DownloadFn = downloadContent
	report, err = eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{})
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
	mock := newRunOnceBidirectionalMock(driveid.New(syncfixture.EngineTestDriveID))
	eng, _ := newTestEngineWithLogger(t, mock, logger)
	mock.DownloadFn = panicATimerAndWaitForTheCancel(eng.Engine)

	done := make(chan error, 1)
	go func() {
		done <- eng.RunWatch(t.Context(), syncdomain.SyncBidirectional, WatchOptions{PollInterval: time.Hour, Debounce: 5 * time.Millisecond})
	}()

	select {
	case err := <-done:
		requireRunEndedByTheTimerPanic(t, capture, err)
	case <-time.After(30 * time.Second):
		require.FailNow(t, "the timer's panic did not end the watch")
	}
	assertNoFailureRecorded(t, eng.Engine)
}

// panicInACommandHelper is a helper goroutine of the command, such as the
// CLI's signal handler or systemd keepalive: its boundary recovers the panic
// and cancels the command with panicguard.AsCancelCause of the panic's error,
// then closes done.
func panicInACommandHelper(cancelCommand context.CancelCauseFunc, done chan<- struct{}) {
	defer close(done)
	defer func() {
		if r := recover(); r != nil {
			cancelCommand(panicguard.AsCancelCause(
				panicguard.Recovered(slog.New(slog.DiscardHandler), "in the signal handler", r)))
		}
	}()

	panic("handler exploded")
}

// Validates: R-6.3.10, R-6.8.29
//
// A panic in a helper goroutine of the command cancels the command's context
// with a cause that answers to context.Canceled as well as to the panic. The
// download that cancel interrupts fails the way the transports fail a
// canceled request, with that cause in place of context.Canceled, and ends as
// at any other stop: nothing counted as failed, no retry work, no blocked
// scope, and the next sync downloads the file. A cause that answered only to
// the panic would leave the download as a failure kept for retry.
func TestRunOnce_AHelperPanicOfTheCommandEndsTheActionInFlightAsAShutdown(t *testing.T) {
	t.Parallel()

	mock := newRunOnceBidirectionalMock(driveid.New(syncfixture.EngineTestDriveID))
	downloadContent := mock.DownloadFn
	eng, syncRoot := newTestEngine(t, mock)
	commandCtx, cancelCommand := context.WithCancelCause(t.Context())
	defer cancelCommand(nil)
	helperDone := make(chan struct{})
	var helperStarted atomic.Bool
	mock.DownloadFn = func(ctx context.Context, _ driveid.ID, _ string, _ io.Writer) (int64, error) {
		if helperStarted.CompareAndSwap(false, true) {
			go panicInACommandHelper(cancelCommand, helperDone)
		}
		<-ctx.Done()

		return 0, fmt.Errorf("request canceled: %w", context.Cause(ctx))
	}

	report, err := eng.RunOnce(commandCtx, syncdomain.SyncBidirectional, RunOptions{})
	require.True(t, helperStarted.Load(), "the download ran")
	<-helperDone

	require.ErrorIs(t, err, context.Canceled, "the run ends as stopped")
	require.ErrorIs(t, context.Cause(commandCtx), panicguard.ErrPanic, "the command keeps the panic")
	require.NotNil(t, report, "RunOnce returns the report it has beside the error")
	assert.Zero(t, report.Failed, "errors: %v", report.Errors)
	assert.Zero(t, report.Succeeded)
	assertNoFailureRecorded(t, eng.Engine)

	mock.DownloadFn = downloadContent
	report, err = eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, report.Succeeded, "the next sync redoes the action the stop interrupted")
	assert.FileExists(t, filepath.Join(syncRoot, "remote.txt"))
}

// panicThroughTheBoundary panics inside clock's timer boundary, as a timer
// callback the clock runs would.
func panicThroughTheBoundary(clock syncdomain.RealClock, value string) {
	defer clock.RecoverTimerPanic()

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
	eng := &Engine{wallClock: syncdomain.NewRealClock(logger, &syncdomain.TimerPanics{})}
	bound := eng.wallClock
	_, end := eng.beginRun(t.Context())
	require.NoError(t, end(nil))

	require.NotPanics(t, func() { panicThroughTheBoundary(bound, "late timer exploded") }, "after its run returned")
	require.NotPanics(t, func() { panicThroughTheBoundary(syncdomain.NewRealClock(logger, nil), "unbound timer exploded") },
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
// one real clock, in newEngine through NewRealClock, and every observer it
// builds runs on it: the observer constructors are called only from the two
// engine methods that set the clock. The constructors' own zero clock is for
// tests that build an observer by hand.
func TestEngineBuildsOneRealClockAndEveryObserverRunsOnIt(t *testing.T) {
	t.Parallel()

	var paths []string
	for _, dir := range syncTreePackageDirs() {
		found, err := filepath.Glob(filepath.Join(dir, "*.go"))
		require.NoError(t, err)
		paths = append(paths, found...)
	}

	fset := token.NewFileSet()
	var literals, clocks, constructorCalls []string
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
					if nameOf(n.Type) == "RealClock" && len(n.Elts) > 0 {
						literals = append(literals, owner)
					}
				case *ast.CallExpr:
					switch name := nameOf(n.Fun); name {
					case "NewRealClock":
						clocks = append(clocks, owner)
					case "NewLocalObserver", "NewRemoteObserver":
						constructorCalls = append(constructorCalls, name+" from "+owner)
					}
				}
				return true
			})
		}
	}

	assert.Equal(t, []string{"NewRealClock"}, literals, "a real clock with timer panics is built only by NewRealClock")
	assert.Equal(t, []string{"newEngine"}, clocks, "one real clock with the engine's timer panics, built in newEngine")
	assert.ElementsMatch(t,
		[]string{"NewLocalObserver from observer", "NewRemoteObserver from observer"},
		constructorCalls, "every observer the engine builds gets the engine's real clock")
}

// nameOf is the name an expression ends in, qualified or not: Name for both
// Name and pkg.Name, "" for any other expression.
func nameOf(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return e.Sel.Name
	}

	return ""
}
