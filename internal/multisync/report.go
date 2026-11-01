package multisync

import (
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

type MountStartupStatus string

const (
	MountStartupRunnable          MountStartupStatus = "runnable"
	MountStartupPaused            MountStartupStatus = "paused"
	MountStartupIncompatibleStore MountStartupStatus = "incompatible_store"
	MountStartupFatal             MountStartupStatus = "fatal"
)

type MountProjectionKind string

const (
	MountProjectionStandalone MountProjectionKind = "standalone"
	MountProjectionChild      MountProjectionKind = "child"
)

type MountIdentity struct {
	MountID        string
	ParentMountID  string
	ProjectionKind MountProjectionKind
	CanonicalID    driveid.CanonicalID
}

func (id *MountIdentity) Label() string {
	if id == nil {
		return ""
	}
	if id.MountID != "" {
		return id.MountID
	}
	if !id.CanonicalID.IsZero() {
		return id.CanonicalID.String()
	}

	return ""
}

func (id *MountIdentity) IsStandalone() bool {
	if id == nil {
		return false
	}

	return id.ProjectionKind == MountProjectionStandalone && !id.CanonicalID.IsZero()
}

// MountStartupResult captures per-mount startup eligibility before any one-shot
// pass or watch runner actually runs. It keeps expected startup policy separate
// from completed sync reports.
type MountStartupResult struct {
	SelectionIndex int
	Identity       MountIdentity
	DisplayName    string
	Status         MountStartupStatus
	Err            error
}

func classifyMountStartupError(err error) MountStartupStatus {
	if err == nil {
		return MountStartupRunnable
	}
	if isIncompatibleStoreStartupError(err) {
		return MountStartupIncompatibleStore
	}

	return MountStartupFatal
}

func isIncompatibleStoreStartupError(err error) bool {
	return err != nil && syncengine.IsStateStoreIncompatible(err)
}

type StartupSelectionSummary struct {
	Results []MountStartupResult
}

func summarizeStartupResults(results []MountStartupResult) StartupSelectionSummary {
	return StartupSelectionSummary{
		Results: append([]MountStartupResult(nil), results...),
	}
}

func (s StartupSelectionSummary) SelectedCount() int {
	return len(s.Results)
}

func (s StartupSelectionSummary) PausedCount() int {
	count := 0
	for i := range s.Results {
		if s.Results[i].Status == MountStartupPaused {
			count++
		}
	}

	return count
}

func (s StartupSelectionSummary) AllPaused() bool {
	return len(s.Results) > 0 && s.PausedCount() == len(s.Results)
}

// PausedResults are the selected drives that are paused: not running, not
// failed, waiting for a resume.
func (s StartupSelectionSummary) PausedResults() []MountStartupResult {
	paused := make([]MountStartupResult, 0, len(s.Results))
	for i := range s.Results {
		if s.Results[i].Status == MountStartupPaused {
			paused = append(paused, s.Results[i])
		}
	}

	return paused
}

func (s StartupSelectionSummary) SkippedResults() []MountStartupResult {
	skipped := make([]MountStartupResult, 0, len(s.Results))
	for i := range s.Results {
		if s.Results[i].Status == MountStartupRunnable || s.Results[i].Status == MountStartupPaused {
			continue
		}
		skipped = append(skipped, s.Results[i])
	}

	return skipped
}

type StartupWarning struct {
	Summary StartupSelectionSummary
}

type RunOnceResult struct {
	Startup StartupSelectionSummary
	Reports []*MountReport
}

// MountReport summarizes the result of a single mount's sync run.
// Err and Report are mutually exclusive: when Err is set, Report is nil.
type MountReport struct {
	SelectionIndex int
	Identity       MountIdentity
	DisplayName    string
	Report         *syncengine.Report
	Err            error
}

type WatchStartupError struct {
	Summary StartupSelectionSummary
}

func (e *WatchStartupError) Error() string {
	if e == nil || e.Summary.SelectedCount() == 0 {
		return "no drives could start syncing; run 'onedrive-go status' for details"
	}
	failures := e.Summary.SkippedResults()
	if len(failures) == 1 {
		failure := failures[0]
		name := failure.DisplayName
		if name == "" {
			name = failure.Identity.Label()
		}
		return fmt.Sprintf("%s could not start syncing: %v", name, failure.Err)
	}

	return fmt.Sprintf("%d drives could not start syncing; run 'onedrive-go status' for details", len(failures))
}

// MountActivity is one finished file action together with the mount it ran
// in, so a caller watching several mounts can say where it happened.
type MountActivity struct {
	Identity    MountIdentity
	DisplayName string
	SyncRoot    string
	Activity    syncengine.Activity
}

func mountActivityHook(mount *mountSpec, hook func(MountActivity)) func(syncengine.Activity) {
	identity := mount.identity()
	displayName := mount.displayName()
	syncRoot := mount.syncRoot()

	return func(activity syncengine.Activity) {
		hook(MountActivity{
			Identity:    identity,
			DisplayName: displayName,
			SyncRoot:    syncRoot,
			Activity:    activity,
		})
	}
}
