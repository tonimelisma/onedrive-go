package multisync

import (
	"errors"
	"fmt"
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
// drain. A clean drain is acknowledged to the parent, which then removes the
// folder's whole local tree, so the drain is clean only when the sync engine
// says that tree may go (Report.LocalTreeHolds): the run left nothing, was
// not a dry run, holds no delete of a local file and left no file an
// interrupted removal set aside. A run that returned an error is never clean.
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

// localTreeHoldsDetail says, in counts, what keeps a removed shared folder's
// local copy.
func localTreeHoldsDetail(holds syncengine.LocalTreeHolds) string {
	var parts []string
	if holds.NoReport {
		parts = append(parts, "the final sync produced no report")
	}
	if holds.DryRun {
		parts = append(parts, "the final sync was a dry run")
	}
	if holds.Left.Failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", holds.Left.Failed))
	}
	if holds.Left.WillRetry > 0 {
		parts = append(parts, fmt.Sprintf("%d to be retried on the next sync", holds.Left.WillRetry))
	}
	if holds.Left.StillOpen > 0 {
		parts = append(parts, fmt.Sprintf("%d still not synced", holds.Left.StillOpen))
	}
	if holds.HeldLocalDeletes > 0 {
		parts = append(parts, fmt.Sprintf("%d local %s held for confirmation",
			holds.HeldLocalDeletes, countNoun(holds.HeldLocalDeletes, "delete", "deletes")))
	}
	if holds.SetAsideFiles > 0 {
		parts = append(parts, fmt.Sprintf("%d %s set aside by an interrupted removal",
			holds.SetAsideFiles, countNoun(holds.SetAsideFiles, "file", "files")))
	}

	return "child sync: " + strings.Join(parts, "; ")
}

// countNoun is singular for a count of one and plural otherwise.
func countNoun(n int, singular, plural string) string {
	if n == 1 {
		return singular
	}

	return plural
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
