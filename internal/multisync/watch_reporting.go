package multisync

import (
	"fmt"
	"log/slog"
	"strconv"
	"time"
)

const (
	// runnerRestartBaseDelay is the wait before restarting a watch runner
	// after its first failure. Each further consecutive failure doubles it.
	runnerRestartBaseDelay = 5 * time.Second
	// runnerRestartMaxDelay caps the restart wait. It matches the default
	// reconcile interval, so a persistently failing drive is retried about as
	// often as everything else is re-checked.
	runnerRestartMaxDelay = 5 * time.Minute
	// runnerHealthyRunDuration is how long a runner must stay up before its
	// exit counts as a fresh failure rather than another step of the same
	// one. A runner that ran this long has recovered, so the backoff and the
	// "already reported" memory start over.
	runnerHealthyRunDuration = runnerRestartMaxDelay
)

// watchReportState is what a watch runtime remembers about the failures it has
// already reported and the runners it is waiting to restart. Watch passes
// repeat on every reconcile tick, reload, and runner event; without this
// memory every pass re-announced every persistent failure.
//
// Ownership: the RunWatch select-loop goroutine is the only reader and
// writer. Runner goroutines and control-socket handlers never touch it, so it
// carries no lock. The state is derived and disposable: losing it only means
// one condition gets reported once more.
type watchReportState struct {
	// announcedStartFailures maps a startup-result key to the fingerprint
	// (status plus error text) last passed to StartWarning.
	announcedStartFailures map[string]string
	// changeWarnings maps a warning key to the error text last logged at WARN.
	changeWarnings map[string]string
	// restarts holds backoff state for runners that exited with an error.
	restarts map[mountID]*runnerRestartState
}

// runnerRestartState tracks one runner's consecutive failures.
type runnerRestartState struct {
	consecutiveFailures int
	notBefore           time.Time
	lastErr             string
}

func newWatchReportState() *watchReportState {
	return &watchReportState{
		announcedStartFailures: make(map[string]string),
		changeWarnings:         make(map[string]string),
		restarts:               make(map[mountID]*runnerRestartState),
	}
}

// watchReports returns the watch runtime's report memory, creating it on first
// use. See watchReportState for the single-goroutine ownership rule.
func (o *Orchestrator) watchReports() *watchReportState {
	if o.watchReportMemory == nil {
		o.watchReportMemory = newWatchReportState()
	}
	return o.watchReportMemory
}

func (o *Orchestrator) clockNow() time.Time {
	if o.now == nil {
		return time.Now()
	}
	return o.now()
}

// startAnnounceScope says whether a pass looked at every selected mount, so
// that a mount missing from its results has stopped failing.
type startAnnounceScope int

const (
	// startAnnouncePartial passes cover one parent's children only.
	startAnnouncePartial startAnnounceScope = iota
	// startAnnounceFull passes cover the whole selected mount set.
	startAnnounceFull
)

// emitStartWarning tells the CLI about mounts that failed to start, but only
// about failures it has not announced yet or whose reason changed. A mount
// that starts, is paused, or (after a full pass) no longer appears among the
// failures is forgotten, so a later failure is announced again.
func (o *Orchestrator) emitStartWarning(results []MountStartupResult, scope startAnnounceScope) {
	if o == nil || o.cfg == nil {
		return
	}

	reports := o.watchReports()
	seen := make(map[string]struct{}, len(results))
	fresh := make([]MountStartupResult, 0, len(results))
	for i := range results {
		result := &results[i]
		key := startupResultKey(result)
		seen[key] = struct{}{}
		if result.Status == MountStartupRunnable || result.Status == MountStartupPaused {
			delete(reports.announcedStartFailures, key)
			continue
		}
		fingerprint := startupResultFingerprint(result)
		if reports.announcedStartFailures[key] == fingerprint {
			o.logger.Debug("drive still cannot start; already reported",
				slog.String("mount_id", key),
				slog.String("status", string(result.Status)),
			)
			continue
		}
		reports.announcedStartFailures[key] = fingerprint
		fresh = append(fresh, *result)
	}

	if scope == startAnnounceFull {
		for key := range reports.announcedStartFailures {
			if _, ok := seen[key]; ok {
				continue
			}
			// A runner waiting out its restart backoff makes no start
			// attempt and so produces no result; it has not recovered.
			if o.runnerRestartPending(mountID(key)) {
				continue
			}
			delete(reports.announcedStartFailures, key)
		}
	}

	if len(fresh) == 0 || o.cfg.StartWarning == nil {
		return
	}
	o.cfg.StartWarning(StartupWarning{Summary: summarizeStartupResults(fresh)})
}

// startupResultKey identifies the mount a startup result is about. Config
// failures can arrive before a mount ID exists, so it falls back to the
// canonical ID and then to the selection slot.
func startupResultKey(result *MountStartupResult) string {
	if result.Identity.MountID != "" {
		return result.Identity.MountID
	}
	if !result.Identity.CanonicalID.IsZero() {
		return result.Identity.CanonicalID.String()
	}
	return "selection:" + strconv.Itoa(result.SelectionIndex) + ":" + result.DisplayName
}

func startupResultFingerprint(result *MountStartupResult) string {
	if result.Err == nil {
		return string(result.Status)
	}
	return string(result.Status) + "\x00" + result.Err.Error()
}

// warnOnChange logs message at WARN the first time key fails and again only
// when the error text changes; repeats of the same failure go to DEBUG. Call
// clearWarnOnChange when the condition clears so a recurrence is reported.
func (o *Orchestrator) warnOnChange(key string, message string, err error) {
	reports := o.watchReports()
	text := err.Error()

	if reports.changeWarnings[key] == text {
		o.logger.Debug(message+" (unchanged; already reported)", slog.Any("error", err))
		return
	}
	reports.changeWarnings[key] = text
	o.logger.Warn(message, slog.Any("error", err))
}

func (o *Orchestrator) clearWarnOnChange(key string) {
	delete(o.watchReports().changeWarnings, key)
}

// recordRunnerFailure updates a runner's backoff after it exited with err and
// returns how long to wait before restarting it, plus whether this failure is
// new (first since the runner was last healthy, or a different error) and so
// worth a warning.
func (o *Orchestrator) recordRunnerFailure(id mountID, startedAt time.Time, err error) (time.Duration, bool) {
	reports := o.watchReports()
	now := o.clockNow()
	state := reports.restarts[id]
	if state == nil || (!startedAt.IsZero() && now.Sub(startedAt) >= runnerHealthyRunDuration) {
		state = &runnerRestartState{}
		reports.restarts[id] = state
	}

	state.consecutiveFailures++
	delay := runnerRestartDelay(state.consecutiveFailures)
	state.notBefore = now.Add(delay)

	text := err.Error()
	isNew := state.lastErr != text
	state.lastErr = text

	return delay, isNew
}

// runnerRestartDelay doubles from runnerRestartBaseDelay per consecutive
// failure, capped at runnerRestartMaxDelay.
func runnerRestartDelay(consecutiveFailures int) time.Duration {
	delay := runnerRestartBaseDelay
	for i := 1; i < consecutiveFailures; i++ {
		delay *= 2
		if delay >= runnerRestartMaxDelay {
			return runnerRestartMaxDelay
		}
	}
	return delay
}

// runnerRestartPending reports whether id is still waiting out its backoff.
func (o *Orchestrator) runnerRestartPending(id mountID) bool {
	state := o.watchReports().restarts[id]
	return state != nil && o.clockNow().Before(state.notBefore)
}

// nextRunnerRestartDelay returns the time until the earliest pending restart.
func (o *Orchestrator) nextRunnerRestartDelay() (time.Duration, bool) {
	now := o.clockNow()
	var next time.Duration
	found := false
	for _, state := range o.watchReports().restarts {
		if !now.Before(state.notBefore) {
			continue
		}
		wait := state.notBefore.Sub(now)
		if !found || wait < next {
			next = wait
			found = true
		}
	}
	return next, found
}

// releaseRunnerRestartWaits lets every backed-off runner start at the next
// pass. A reload is an explicit request to apply current config now, so it
// overrides the waits; the failure counts and the already-reported errors are
// kept so an unchanged failure is not announced again.
func (o *Orchestrator) releaseRunnerRestartWaits() {
	for _, state := range o.watchReports().restarts {
		state.notBefore = time.Time{}
	}
}

// forgetRunnerRestart drops backoff state for a mount that is no longer
// selected, so re-adding it later starts clean.
func (o *Orchestrator) forgetRunnerRestart(id mountID) {
	delete(o.watchReports().restarts, id)
}

// humanDelay renders a restart delay in the units the output guidelines ask
// for: "5 s", "2 min", "2 min 30 s".
func humanDelay(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%d s", int(d/time.Second))
	}
	minutes := int(d / time.Minute)
	seconds := int((d % time.Minute) / time.Second)
	if seconds == 0 {
		return fmt.Sprintf("%d min", minutes)
	}
	return fmt.Sprintf("%d min %d s", minutes, seconds)
}
