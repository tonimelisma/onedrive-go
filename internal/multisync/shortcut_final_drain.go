package multisync

import (
	"errors"
	"fmt"

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
