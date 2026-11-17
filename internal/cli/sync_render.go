package cli

import (
	"errors"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/multisync"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

// errSyncLeftWork is the cause of a one-shot sync that finished but left
// files unsynced (R-6.6.28). It carries no exit-status sentinel, so the run
// exits 1: running again may finish the work.
var errSyncLeftWork = errors.New("sync left work unfinished")

// runOnceResultError is the one-shot command's final error: the drives and
// shared folders that failed or were skipped; otherwise those that left work
// (left, keyed by mount ID, each the report's own Report.LeftWork, from
// syncConsole.Summary, so the exit status and the summaries cannot
// disagree); and a drive whose sync state did not close after the pass
// (RunOnceResult.Stuck), which fails the command even when every drive
// synced, so the process exits and the stuck engine's locks go. A drive error
// comes first, so its exit status (77, 78) wins.
func runOnceResultError(result multisync.RunOnceResult, left map[string]syncengine.LeftWork) error {
	drivesErr := runOnceDrivesError(result)
	if drivesErr == nil {
		drivesErr = runOnceLeftWorkError(result, left)
	}
	if result.Stuck == nil {
		return drivesErr
	}

	return errors.Join(drivesErr, result.Stuck)
}

// runOnceLeftWorkError fails a run that left work on any mount it ran. Each
// mount's summary line already said what; the error says where to look.
func runOnceLeftWorkError(result multisync.RunOnceResult, left map[string]syncengine.LeftWork) error {
	kinds := projectionKindByMount(result.Reports)
	var leftMounts selectionTally
	files := 0
	for mountID, counts := range left {
		if counts.Any() {
			leftMounts.add(tallyKey{mountID: mountID}, kinds[mountID])
			files += counts.Total()
		}
	}
	if leftMounts.empty() {
		return nil
	}

	if result.Startup.StartupResultCount() <= 1 {
		return newPresentedError(fmt.Sprintf("sync left %s unsynced; run 'onedrive-go status' for details",
			countOf(files, "file")), errSyncLeftWork)
	}

	return newPresentedError(leftMounts.phrase(result.Startup)+" left files unsynced (see above)", errSyncLeftWork)
}

func runOnceDrivesError(result multisync.RunOnceResult) error {
	if result.Startup.AllPaused() {
		return fmt.Errorf("all selected mounts are paused — run 'onedrive-go resume' to unpause")
	}

	var firstErr error
	var failed selectionTally

	skipped := result.Startup.SkippedResults()
	for i := range skipped {
		startup := &skipped[i]
		failed.add(newTallyKey(startup.Identity.MountID, startup.SelectionIndex), startup.Identity.ProjectionKind)
		if firstErr == nil {
			firstErr = newPresentedError(formatStartupResultMessage(startup), startup.Err)
		}
	}

	for _, dr := range result.Reports {
		if dr == nil || dr.Err == nil {
			continue
		}
		failed.add(newTallyKey(dr.Identity.MountID, dr.SelectionIndex), dr.Identity.ProjectionKind)
		if firstErr == nil {
			firstErr = formatStateStoreIncompatibleError(&dr.Identity, dr.Err)
		}
	}

	if failed.empty() {
		return nil
	}

	// One result means the console printed no names and left the error to
	// the command, so it prints once.
	if result.Startup.StartupResultCount() <= 1 {
		return firstErr
	}

	// The per-mount errors were already printed with their names, so the
	// returned message only counts them; the first error stays reachable for
	// classification and the exit code.
	// The verb agrees with what is counted, drives and shared folders together.
	verb := "were"
	if failed.count() == 1 {
		verb = "was"
	}

	return newPresentedError(failed.phrase(result.Startup)+" failed or "+verb+" skipped (see above)", firstErr)
}

// selectionTally counts the mounts a run's closing sentence is about, each
// once (tallyKey): the configured drives apart from the shared folders below
// them, because a shared folder is not one of the drives the user selected.
type selectionTally struct {
	drives        map[tallyKey]struct{}
	sharedFolders map[tallyKey]struct{}
}

// tallyKey names one tallied result: its mount ID, or, for a result that
// carries none, its selection index, so two such results count as two.
type tallyKey struct {
	mountID        string
	selectionIndex int
}

func newTallyKey(mountID string, selectionIndex int) tallyKey {
	if mountID != "" {
		return tallyKey{mountID: mountID}
	}

	return tallyKey{selectionIndex: selectionIndex}
}

func (t *selectionTally) add(key tallyKey, kind multisync.MountProjectionKind) {
	if kind == multisync.MountProjectionChild {
		t.sharedFolders = addTallyKey(t.sharedFolders, key)
		return
	}
	t.drives = addTallyKey(t.drives, key)
}

func (t *selectionTally) empty() bool {
	return t.count() == 0
}

// count is how many mounts are tallied, drives and shared folders together.
func (t *selectionTally) count() int {
	return len(t.drives) + len(t.sharedFolders)
}

// phrase counts the tallied mounts against what the run selected: "1 of 2
// selected drives", "1 of 2 shared folders in 1 selected drive" when only
// shared folders are tallied, or both joined by "and".
func (t *selectionTally) phrase(startup multisync.StartupSelectionSummary) string {
	selectedDrives := countOf(startup.SelectedDriveCount(), "selected drive")
	if len(t.sharedFolders) == 0 {
		return fmt.Sprintf("%d of %s", len(t.drives), selectedDrives)
	}

	folderPart := fmt.Sprintf("%d of %s", len(t.sharedFolders), countOf(sharedFolderCount(startup), "shared folder"))
	if len(t.drives) == 0 {
		return folderPart + " in " + selectedDrives
	}

	return fmt.Sprintf("%d of %s and %s", len(t.drives), selectedDrives, folderPart)
}

func addTallyKey(set map[tallyKey]struct{}, key tallyKey) map[tallyKey]struct{} {
	if set == nil {
		set = make(map[tallyKey]struct{})
	}
	set[key] = struct{}{}

	return set
}

// sharedFolderCount is how many shared folders below the selected drives the
// run's startup results name, each once.
func sharedFolderCount(startup multisync.StartupSelectionSummary) int {
	folders := make(map[string]struct{}, len(startup.Results))
	for i := range startup.Results {
		if startup.Results[i].Identity.ProjectionKind == multisync.MountProjectionChild {
			folders[startup.Results[i].Identity.MountID] = struct{}{}
		}
	}

	return len(folders)
}

// projectionKindByMount says, for each mount a report names, whether it is a
// drive or a shared folder below one.
func projectionKindByMount(reports []*multisync.MountReport) map[string]multisync.MountProjectionKind {
	kinds := make(map[string]multisync.MountProjectionKind, len(reports))
	for _, report := range reports {
		if report != nil {
			kinds[report.Identity.MountID] = report.Identity.ProjectionKind
		}
	}

	return kinds
}

func formatMountReportErrorMessage(dr *multisync.MountReport) string {
	if dr == nil || dr.Err == nil {
		return ""
	}

	return formatStateStoreIncompatibleMessage(&dr.Identity, dr.Err)
}
