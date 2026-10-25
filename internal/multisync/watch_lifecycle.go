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
	// WatchReloaded: a reload finished. Err is set when nothing could be
	// applied and the previous settings were kept.
	WatchReloaded
	// WatchStopping: the watch loop is shutting down.
	WatchStopping
)

// WatchLifecycleEvent is delivered to OrchestratorConfig.WatchLifecycle.
type WatchLifecycleEvent struct {
	Phase WatchLifecyclePhase
	// SyncRoots are the local roots of the standalone drives running after
	// Ready or a successful Reloaded, sorted.
	SyncRoots []string
	// Skipped are the selected drives that could not start, after Ready or
	// a successful Reloaded.
	Skipped []MountStartupResult
	// Err is set on a Reloaded that kept the previous settings; SyncRoots
	// and Skipped are then empty because nothing changed.
	Err error
}

// emitWatchLifecycle reports a transition. It runs on the RunWatch goroutine,
// which owns runners, so the hook sees a consistent mount set.
func (o *Orchestrator) emitWatchLifecycle(
	phase WatchLifecyclePhase,
	runners map[mountID]*watchRunner,
	startResults []MountStartupResult,
	err error,
) {
	if o.cfg == nil || o.cfg.WatchLifecycle == nil {
		return
	}

	event := WatchLifecycleEvent{Phase: phase, Err: err}
	if err == nil && (phase == WatchReady || phase == WatchReloaded) {
		for _, wr := range runners {
			if wr.mount.projectionKind() == MountProjectionStandalone {
				event.SyncRoots = append(event.SyncRoots, wr.mount.syncRoot())
			}
		}
		sort.Strings(event.SyncRoots)
		event.Skipped = summarizeStartupResults(startResults).SkippedResults()
	}

	o.cfg.WatchLifecycle(event)
}
