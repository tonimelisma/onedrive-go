package multisync

import "sort"

// WatchLifecyclePhase is one transition of a running watch, reported so the
// CLI can tell a service manager (sd_notify READY/RELOADING/STOPPING).
type WatchLifecyclePhase int

const (
	// WatchReady: startup finished and every drive that could start is
	// running its watchers.
	WatchReady WatchLifecyclePhase = iota + 1
	// WatchReloading: a config reload started.
	WatchReloading
	// WatchReloaded: a reload finished; Reload says what it did. A reload
	// that ends the watch, because a drive it had to stop did not stop,
	// reports no Reloaded; WatchStopping follows.
	WatchReloaded
	// WatchStopping: the watch loop is shutting down.
	WatchStopping
)

// ReloadOutcome is what a finished reload did with the config. A consumer
// switches on it rather than inferring the outcome from Err, so a new kind of
// outcome cannot be reported as one it is not.
type ReloadOutcome int

const (
	// ReloadApplied: the config was applied; SyncRoots, Skipped and Paused
	// describe the drives under it.
	ReloadApplied ReloadOutcome = iota + 1
	// ReloadKeptPreviousSettings: the config would not load or compile, so
	// nothing changed and the drives keep their previous settings; Err says
	// why.
	ReloadKeptPreviousSettings
)

// WatchLifecycleEvent is delivered to OrchestratorConfig.WatchLifecycle.
type WatchLifecycleEvent struct {
	Phase WatchLifecyclePhase
	// Reload is set on WatchReloaded only.
	Reload ReloadOutcome
	// SyncRoots are the local roots of the standalone drives running after
	// Ready or an applied Reloaded, sorted.
	SyncRoots []string
	// Skipped are the selected drives that could not start, after Ready or
	// an applied Reloaded.
	Skipped []MountStartupResult
	// Paused are the selected drives that are paused, after Ready or an
	// applied Reloaded. A watch whose drives are all paused is Ready with no
	// SyncRoots and every drive here.
	Paused []MountStartupResult
	// Err is the reason of a Reloaded that kept the previous settings.
	Err error
}

// describesDrives reports whether the event carries the drive set: Ready,
// and a Reloaded that applied the config.
func (e *WatchLifecycleEvent) describesDrives() bool {
	return e.Phase == WatchReady || (e.Phase == WatchReloaded && e.Reload == ReloadApplied)
}

// emitWatchLifecycle reports a transition: event carries its phase, and for
// a Reloaded its outcome and error; the drive set is filled in here. It runs
// on the RunWatch goroutine, which owns runners, so the hook sees a
// consistent mount set.
func (o *Orchestrator) emitWatchLifecycle(
	event WatchLifecycleEvent,
	runners map[mountID]*watchRunner,
	startResults []MountStartupResult,
) {
	if o.cfg == nil || o.cfg.WatchLifecycle == nil {
		return
	}

	if event.describesDrives() {
		for _, wr := range runners {
			if wr.mount.projectionKind() == MountProjectionStandalone {
				event.SyncRoots = append(event.SyncRoots, wr.mount.syncRoot())
			}
		}
		sort.Strings(event.SyncRoots)
		summary := summarizeStartupResults(startResults)
		event.Skipped = summary.SkippedResults()
		event.Paused = summary.PausedResults()
	}

	o.cfg.WatchLifecycle(event)
}
