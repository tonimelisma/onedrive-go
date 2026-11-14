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

// SelectedDriveCount is how many configured drives the results name, each
// once however many results name it: a drive whose shared folders could not
// be set up has a second result beside its own. A shared folder below a
// drive is not a drive, and a result that names no drive (a failure that
// stopped the run before any drive started) counts none.
func (s StartupSelectionSummary) SelectedDriveCount() int {
	drives := make(map[string]struct{}, len(s.Results))
	for i := range s.Results {
		identity := &s.Results[i].Identity
		if identity.ProjectionKind != MountProjectionStandalone || identity.MountID == "" {
			continue
		}
		drives[identity.MountID] = struct{}{}
	}

	return len(drives)
}

// StartupResultCount is how many results there are: one for each drive and
// each shared folder of the run, a second one for a drive whose shared
// folders could not be set up, or the single result of a failure that
// stopped the run before any drive started. More than one means the run's
// output names what each of its lines is about.
func (s StartupSelectionSummary) StartupResultCount() int {
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
	// Stuck is set when a drive's engine did not close within its bound after
	// the pass (it wraps ErrSyncStuck) or its close panicked (it wraps
	// panicguard.ErrPanic). The reports stand, but the command fails, so the
	// process exits and the engine's locks are released.
	Stuck error
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
	if e == nil || e.Summary.StartupResultCount() == 0 {
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

	return countDrivesAndSharedFolders(failures) + " could not start syncing; run 'onedrive-go status' for details"
}

// countDrivesAndSharedFolders counts the drives and the shared folders below
// them that results name, apart and each once: "2 drives", "1 shared
// folder", or both joined by "and". A drive whose shared folders could not be
// set up has a second result that carries the drive's own identity, so it
// counts once; a result that names no mount counts once by its selection
// index.
func countDrivesAndSharedFolders(results []MountStartupResult) string {
	type mountKey struct {
		mountID        string
		selectionIndex int
	}
	drives := make(map[mountKey]struct{}, len(results))
	sharedFolders := make(map[mountKey]struct{}, len(results))
	for i := range results {
		key := mountKey{mountID: results[i].Identity.MountID}
		if key.mountID == "" {
			key.selectionIndex = results[i].SelectionIndex
		}
		if results[i].Identity.ProjectionKind == MountProjectionChild {
			sharedFolders[key] = struct{}{}
			continue
		}
		drives[key] = struct{}{}
	}

	drivePart := formatMountCount(len(drives), "drive", "drives")
	folderPart := formatMountCount(len(sharedFolders), "shared folder", "shared folders")
	switch {
	case len(sharedFolders) == 0:
		return drivePart
	case len(drives) == 0:
		return folderPart
	default:
		return drivePart + " and " + folderPart
	}
}

// formatMountCount is count followed by the singular noun for one and the
// plural otherwise.
func formatMountCount(count int, singular, plural string) string {
	if count == 1 {
		return "1 " + singular
	}

	return fmt.Sprintf("%d %s", count, plural)
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
