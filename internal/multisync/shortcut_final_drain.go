package multisync

import (
	"errors"
	"strings"

	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

type shortcutChildDrainStatus string

const (
	shortcutChildDrainClean           shortcutChildDrainStatus = "clean"
	shortcutChildDrainBlocked         shortcutChildDrainStatus = "blocked"
	shortcutChildDrainRootUnavailable shortcutChildDrainStatus = "root_unavailable"
	shortcutChildDrainFailed          shortcutChildDrainStatus = "failed"
)

type shortcutChildDrainResult struct {
	MountID string
	AckRef  syncengine.ShortcutChildAckRef
	Status  shortcutChildDrainStatus
	Detail  string
}

func classifyShortcutChildDrainResults(
	mounts []*mountSpec,
	reports []*MountReport,
) []shortcutChildDrainResult {
	results := make([]shortcutChildDrainResult, 0)
	for _, mount := range mounts {
		if mount == nil || !mount.isFinalDrainChild() {
			continue
		}
		mountID := mount.id().String()
		result := shortcutChildDrainResult{
			MountID: mountID,
			AckRef:  mount.shortcutChildAckRef(),
		}
		report := mountReportForID(reports, mountID)
		result.Status, result.Detail = classifyShortcutChildDrainReport(report)
		results = append(results, result)
	}
	return results
}

func mountReportForID(reports []*MountReport, mountID string) *MountReport {
	for _, report := range reports {
		if report != nil && report.Identity.MountID == mountID {
			return report
		}
	}
	return nil
}

// classifyShortcutChildDrainReport decides a removed shared folder's final
// drain. The drain's last step released the folder's local tree by the local
// delete rule (RunOptions.ReleaseLocalTree), and a clean drain is acknowledged
// to the parent, which then removes the emptied alias root, so the drain is
// clean only when the sync engine says nothing holds the tree
// (Report.LocalTreeHolds): the run left nothing, was not a dry run, holds no
// delete of a local file, left no file an interrupted removal set aside and
// released the whole tree. A run that returned an error is never clean.
func classifyShortcutChildDrainReport(report *MountReport) (shortcutChildDrainStatus, string) {
	if report == nil {
		return shortcutChildDrainFailed, "final-drain child did not produce a mount report"
	}
	if report.Err != nil {
		if isMountRootUnavailableReport(report.Err) {
			return shortcutChildDrainRootUnavailable, report.Err.Error()
		}
		return shortcutChildDrainFailed, report.Err.Error()
	}
	if report.Report == nil {
		return shortcutChildDrainFailed, "final-drain child produced no sync report"
	}
	if holds := report.Report.LocalTreeHolds(); holds.Any() {
		return shortcutChildDrainBlocked, localTreeHoldsDetail(holds)
	}
	return shortcutChildDrainClean, ""
}

// localTreeHoldsDetail says what keeps a removed shared folder's local copy,
// in the sentences the sync engine gives for it (LocalTreeHolds.Reasons).
func localTreeHoldsDetail(holds *syncengine.LocalTreeHolds) string {
	return childSyncDetailPrefix + strings.Join(holds.Reasons(), "; ")
}

// childSyncDetailPrefix starts every blocked drain's detail.
const childSyncDetailPrefix = "child sync: "

func cleanShortcutChildDrainResults(results []shortcutChildDrainResult) []shortcutChildDrainResult {
	clean := make([]shortcutChildDrainResult, 0, len(results))
	for _, result := range results {
		if result.Status == shortcutChildDrainClean {
			clean = append(clean, result)
		}
	}
	return clean
}

func isMountRootUnavailableReport(err error) bool {
	return errors.Is(err, syncengine.ErrMountRootUnavailable)
}
