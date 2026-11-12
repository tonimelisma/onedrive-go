package multisync

import (
	"time"

	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

// watchRestartWake is the RunWatch loop's single timer for the earliest
// backed-off runner restart. The loop re-arms it before every select, from
// the absolute restart times in watchReportState, so it never drifts and
// never runs when nothing is waiting.
type watchRestartWake struct {
	orchestrator *Orchestrator
	ch           <-chan time.Time
	cancel       func()
}

func (w *watchRestartWake) arm() {
	w.stop()
	delay, ok := w.orchestrator.nextRunnerRestartDelay()
	if !ok {
		return
	}
	timer := w.orchestrator.restartTimer
	if timer == nil {
		timer = newOneShotTimer
	}
	w.ch, w.cancel = timer(delay)
}

func (w *watchRestartWake) stop() {
	if w.cancel != nil {
		w.cancel()
	}
	w.ch, w.cancel = nil, nil
}

// watchPauseExpiryWake is the RunWatch loop's timer for the earliest timed
// pause to run out. `pause 1h` records paused_until in the config, and the
// expired pause used to be noticed only by the next config reload, which
// nothing scheduled: without a `resume` or an unrelated config change the
// drive stayed paused for good (R-2.8.12). Re-armed before every select from
// the absolute paused_until times, like watchRestartWake.
type watchPauseExpiryWake struct {
	orchestrator *Orchestrator
	ch           <-chan time.Time
	cancel       func()
}

func (w *watchPauseExpiryWake) arm() {
	w.stop()
	delay, ok := w.orchestrator.nextPauseExpiryDelay()
	if !ok {
		return
	}
	timer := w.orchestrator.restartTimer
	if timer == nil {
		timer = newOneShotTimer
	}
	w.ch, w.cancel = timer(delay)
}

func (w *watchPauseExpiryWake) stop() {
	if w.cancel != nil {
		w.cancel()
	}
	w.ch, w.cancel = nil, nil
}

// pauseExpirySlack is added to the wait so the reload reads the pause as
// expired even when the config's second-precision timestamp rounds up.
const pauseExpirySlack = time.Second

// nextPauseExpiryDelay returns how long until the earliest timed pause among
// the configured drives runs out, or false when no timed pause is active.
func (o *Orchestrator) nextPauseExpiryDelay() (time.Duration, bool) {
	if o.cfg == nil || o.cfg.Holder == nil {
		return 0, false
	}
	cfg := o.cfg.Holder.Config()
	if cfg == nil {
		return 0, false
	}
	now := o.clockNow()
	var next time.Duration
	found := false
	for cid := range cfg.Drives {
		drive := cfg.Drives[cid]
		until, ok := drive.PauseExpiry()
		if !ok || !now.Before(until) {
			continue
		}
		wait := until.Sub(now) + pauseExpirySlack
		if !found || wait < next {
			next = wait
			found = true
		}
	}

	return next, found
}

func reconcileWatchInterval(pollInterval time.Duration) time.Duration {
	const defaultReconcileInterval = 5 * time.Minute

	if pollInterval <= 0 {
		return defaultReconcileInterval
	}
	if pollInterval < syncengine.MinPollInterval {
		return syncengine.MinPollInterval
	}

	return pollInterval
}
