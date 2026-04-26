package multisync

import (
	"errors"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/config"
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
	MountID       string
	BindingItemID string
	Status        shortcutChildDrainStatus
	Detail        string
}

func finalDrainMountIDs(topologies map[mountID]syncengine.ShortcutChildTopologyPublication) []string {
	if len(topologies) == 0 {
		return nil
	}

	ids := make([]string, 0)
	children := sortedPublishedShortcutChildren(topologies)
	for i := range children {
		child := children[i].child
		if child.RunnerAction == syncengine.ShortcutChildActionFinalDrain {
			ids = append(ids, config.ChildMountID(children[i].namespaceID.String(), child.BindingItemID))
		}
	}

	return ids
}

func classifyShortcutChildDrainResults(
	finalDrainMountIDs []string,
	mounts []*mountSpec,
	reports []*MountReport,
) []shortcutChildDrainResult {
	draining := make(map[string]*mountSpec, len(finalDrainMountIDs))
	for _, mountID := range finalDrainMountIDs {
		if mountID == "" {
			continue
		}
		for _, mount := range mounts {
			if mount != nil && mount.mountID.String() == mountID {
				draining[mountID] = mount
				break
			}
		}
	}
	results := make([]shortcutChildDrainResult, 0, len(draining))
	for mountID, mount := range draining {
		result := shortcutChildDrainResult{
			MountID: mountID,
		}
		if mount != nil {
			result.BindingItemID = mount.bindingItemID
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
	if report.Report.Failed > 0 || len(report.Report.Errors) > 0 {
		return shortcutChildDrainBlocked, fmt.Sprintf("child sync reported %d failed action(s)", report.Report.Failed)
	}
	return shortcutChildDrainClean, ""
}

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
