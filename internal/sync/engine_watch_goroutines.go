package sync

import (
	"context"
	"log/slog"
	"slices"
	"time"
)

// watchGoroutineStopTimeout bounds how long a watch's stop waits for the
// goroutines it started beside its loop once it has canceled them. They end
// on their context within milliseconds; the bound only keeps one that
// ignores its context from holding the watch's return for ever. It is kept
// well under the multisync runner stop bound (watchRunnerStopTimeout, one
// detached store write's OutcomeCommitTimeout of 30 s plus 15 s of slack):
// the drain's one bounded store write plus this wait is 35 s, so the engine
// names its own slow goroutine before the orchestrator gives up on the whole
// runner.
const watchGoroutineStopTimeout = 5 * time.Second

// watchGoroutineKind names, for the stop's Warn, a kind of goroutine the
// watch starts beside its loop.
type watchGoroutineKind string

const (
	watchGoroutineRemoteObserver watchGoroutineKind = "remote observer"
	watchGoroutineLocalObserver  watchGoroutineKind = "local observer"
	watchGoroutineFullRefresh    watchGoroutineKind = "full remote refresh"
	watchGoroutineWakeSource     watchGoroutineKind = "socket.io wake source"
)

// watchGoroutine is one started goroutine: the cancel of its own context, and
// a channel it closes when it returns.
type watchGoroutine struct {
	kind   watchGoroutineKind
	cancel context.CancelFunc
	done   chan struct{}
}

// watchGoroutines owns the goroutines a watch starts beside its loop that
// can write the store or the engine's state: the remote and local observers,
// the full remote refresh, and the socket.io wake source. Each runs under a
// context of its own that stop cancels, and stop waits for each to return,
// within watchGoroutineStopTimeout. The loop's goroutine alone starts them and
// stops them, so the list needs no lock. The zero value is ready to use, and
// stop on a value that started nothing returns at once.
type watchGoroutines struct {
	started []watchGoroutine
	// expired closes once watchGoroutineStopTimeout has passed since the
	// first wait for the goroutines began: the stop's drain, waiting for the
	// observers' exits and a refresh's result, and stop itself share the one
	// bound. timer is its timer; both are nil until a wait begins.
	expired chan struct{}
	timer   syncTimer
}

// deadline is the channel that closes when the bound on waiting for the
// goroutines has passed, arming the bound on the engine's clock at the first
// call.
func (g *watchGoroutines) deadline(clock syncClock) <-chan struct{} {
	if g.expired == nil {
		expired := make(chan struct{})
		g.expired = expired
		g.timer = clock.AfterFunc(watchGoroutineStopTimeout, func() { close(expired) })
	}

	return g.expired
}

// start runs fn on a goroutine of its own, under a context derived from ctx
// that stop cancels. fn returning, normally or through a recovered panic,
// counts as the goroutine's exit.
func (g *watchGoroutines) start(ctx context.Context, kind watchGoroutineKind, fn func(context.Context)) {
	g.forgetExited()
	//nolint:gosec // G118: the cancel is kept in started, which stop calls.
	goroutineCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	g.started = append(g.started, watchGoroutine{kind: kind, cancel: cancel, done: done})

	go runWatchGoroutine(goroutineCtx, done, fn)
}

// runWatchGoroutine runs fn and then closes done. It holds no panic boundary
// of its own: every fn the watch starts defers a recover that hands the panic
// to panicguard.Recovered and reports it to the watch, which a boundary here
// could not do.
func runWatchGoroutine(ctx context.Context, done chan<- struct{}, fn func(context.Context)) {
	defer close(done)
	fn(ctx)
}

// forgetExited drops the goroutines that have already returned, releasing
// their contexts, so a long watch that starts a full refresh at every
// interval keeps only the ones still running.
func (g *watchGoroutines) forgetExited() {
	g.started = slices.DeleteFunc(g.started, func(started watchGoroutine) bool {
		select {
		case <-started.done:
			started.cancel()
			return true
		default:
			return false
		}
	})
}

// stop cancels every started goroutine and waits for each to return. When the
// bound passes first, one Warn names the kinds still running, and stop
// returns all the same: a goroutine that ignores its context must not hold
// the watch's return for ever. Safe to call more than once.
func (g *watchGoroutines) stop(clock syncClock, logger *slog.Logger) {
	defer g.disarm()
	if len(g.started) == 0 {
		return
	}
	started := g.started
	g.started = nil

	for i := range started {
		started[i].cancel()
	}

	expired := g.deadline(clock)

	waiting := true
	var running []string
	for i := range started {
		if waiting {
			select {
			case <-started[i].done:
				continue
			case <-expired:
				waiting = false
			}
		}
		select {
		case <-started[i].done:
		default:
			running = append(running, string(started[i].kind))
		}
	}
	if len(running) == 0 {
		return
	}

	slices.Sort(running)
	logger.Warn("Watch goroutines did not stop in time; the watch stops without them",
		slog.Duration("waited", watchGoroutineStopTimeout),
		slog.Any("still_running", slices.Compact(running)),
	)
}

// disarm stops the bound's timer, so the next wait measures its own.
func (g *watchGoroutines) disarm() {
	if g.timer != nil {
		g.timer.Stop()
	}
	g.expired, g.timer = nil, nil
}
