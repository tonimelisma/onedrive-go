package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/perf"
)

const (
	statusPerfUnavailableNoOwner    = "no active sync owner; check logs"
	statusPerfUnavailableGeneric    = "live perf unavailable; check logs"
	statusPerfUnavailableInactive   = "drive is not active in the current sync owner; check logs"
	statusPerfUnavailableCollecting = "collecting live perf data; check logs"
)

type statusPerfOverlay struct {
	enabled           bool
	ownerPresent      bool
	unavailableReason string
	managedDrives     map[string]struct{}
	snapshots         map[string]perf.Snapshot
}

func loadStatusPerfOverlay(ctx context.Context, showPerf bool) statusPerfOverlay {
	if !showPerf {
		return statusPerfOverlay{}
	}

	probe, err := probeControlOwner(ctx)
	if err != nil {
		return statusPerfOverlay{
			enabled:           true,
			unavailableReason: statusPerfUnavailableGeneric,
		}
	}

	switch probe.state {
	case controlOwnerStateWatchOwner, controlOwnerStateOneShotOwner:
		overlay := statusPerfOverlay{
			enabled:       true,
			ownerPresent:  true,
			managedDrives: make(map[string]struct{}, len(probe.client.status.Drives)),
		}
		for _, canonicalID := range probe.client.status.Drives {
			overlay.managedDrives[canonicalID] = struct{}{}
		}

		response, perfErr := probe.client.perfStatus(ctx)
		if perfErr != nil {
			overlay.unavailableReason = statusPerfUnavailableGeneric
			return overlay
		}

		overlay.snapshots = response.Drives
		return overlay
	case controlOwnerStateNoSocket:
		return statusPerfOverlay{
			enabled:           true,
			unavailableReason: statusPerfUnavailableNoOwner,
		}
	case controlOwnerStatePathUnavailable, controlOwnerStateProbeFailed:
		return statusPerfOverlay{
			enabled:           true,
			unavailableReason: statusPerfUnavailableGeneric,
		}
	default:
		return statusPerfOverlay{
			enabled:           true,
			unavailableReason: statusPerfUnavailableGeneric,
		}
	}
}

func applyStatusPerfOverlay(accounts []statusAccount, overlay statusPerfOverlay) {
	if !overlay.enabled {
		return
	}

	for i := range accounts {
		for j := range accounts[i].Drives {
			drive := &accounts[i].Drives[j]
			drive.SyncState = overlaySyncState(drive.CanonicalID, drive.SyncState, overlay)
		}
	}
}

func overlaySyncState(
	canonicalID string,
	state *syncStateInfo,
	overlay statusPerfOverlay,
) *syncStateInfo {
	snapshot, unavailableReason := overlay.lookup(canonicalID)
	if snapshot == nil && unavailableReason == "" {
		return state
	}

	if state == nil {
		state = &syncStateInfo{}
	}

	state.Perf = snapshot
	state.PerfUnavailableReason = unavailableReason

	return state
}

func (overlay statusPerfOverlay) lookup(canonicalID string) (*perf.Snapshot, string) {
	if !overlay.enabled {
		return nil, ""
	}

	if snapshot, ok := overlay.snapshots[canonicalID]; ok {
		snapshotCopy := snapshot
		return &snapshotCopy, ""
	}

	if !overlay.ownerPresent {
		return nil, overlay.unavailableReason
	}

	if _, ok := overlay.managedDrives[canonicalID]; !ok {
		return nil, statusPerfUnavailableInactive
	}

	if overlay.unavailableReason != "" {
		return nil, overlay.unavailableReason
	}

	return nil, statusPerfUnavailableCollecting
}

func (ss *syncStateInfo) hasPersistentStatusData() bool {
	if ss == nil {
		return false
	}

	return ss.LastSyncTime != "" ||
		ss.LastSyncDuration != "" ||
		ss.FileCount > 0 ||
		ss.IssueCount > 0 ||
		len(ss.IssueGroups) > 0 ||
		ss.PendingSync > 0 ||
		ss.Retrying > 0 ||
		ss.LastError != "" ||
		ss.StateStoreStatus != "" ||
		ss.StateStoreError != "" ||
		ss.StateStoreRecoveryHint != "" ||
		ss.DeleteSafetyTotal > 0 ||
		len(ss.DeleteSafety) > 0 ||
		ss.ConflictsTotal > 0 ||
		len(ss.Conflicts) > 0 ||
		ss.ConflictHistoryTotal > 0 ||
		len(ss.ConflictHistory) > 0 ||
		len(ss.NextActions) > 0 ||
		ss.HeldDeletesWaiting > 0 ||
		ss.ApprovedDeletesWaiting > 0 ||
		ss.QueuedConflictRequests > 0 ||
		ss.ApplyingConflictRequests > 0
}

func printStatusPerfText(w io.Writer, ss *syncStateInfo) error {
	if ss == nil || (ss.Perf == nil && ss.PerfUnavailableReason == "") {
		return nil
	}

	if err := writeln(w); err != nil {
		return err
	}
	if err := writeln(w, "    PERF"); err != nil {
		return err
	}

	if ss.Perf == nil {
		return writef(w, "    Live performance unavailable: %s\n", ss.PerfUnavailableReason)
	}

	snapshot := ss.Perf
	actionCount := snapshot.ExecuteActionCount
	if actionCount == 0 {
		actionCount = snapshot.PlannedActionCount
	}

	lines := []struct {
		label string
		value string
	}{
		{label: "Live", value: formatPerfElapsed(snapshot.DurationMS)},
		{label: "HTTP", value: formatDetailedPerfHTTP(snapshot)},
		{label: "DB", value: fmt.Sprintf(
			"%d tx in %s",
			snapshot.DBTransactionCount,
			formatPerfElapsed(snapshot.DBTransactionTimeMS),
		)},
		{label: "Transfers", value: formatDetailedPerfTransfers(snapshot)},
		{label: "Phases", value: formatDetailedPerfPhases(snapshot)},
		{label: "Activity", value: formatDetailedPerfActivity(snapshot, actionCount)},
	}

	for i := range lines {
		line := lines[i]
		if err := writef(w, "    %-10s %s\n", line.label+":", line.value); err != nil {
			return err
		}
	}

	return nil
}

func formatPerfElapsed(durationMS int64) string {
	duration := time.Duration(durationMS) * time.Millisecond
	if duration <= 0 {
		return "0s"
	}

	return duration.Round(time.Millisecond).String()
}

func formatDetailedPerfHTTP(snapshot *perf.Snapshot) string {
	return fmt.Sprintf(
		"%d req, %d retries, %d transport errors",
		snapshot.HTTPRequestCount,
		snapshot.HTTPRetryCount,
		snapshot.HTTPTransportErrors,
	)
}

func formatDetailedPerfTransfers(snapshot *perf.Snapshot) string {
	return fmt.Sprintf(
		"down %d (%s), up %d (%s)",
		snapshot.DownloadCount,
		formatSize(snapshot.DownloadBytes),
		snapshot.UploadCount,
		formatSize(snapshot.UploadBytes),
	)
}

func formatDetailedPerfPhases(snapshot *perf.Snapshot) string {
	return fmt.Sprintf(
		"observe %s, plan %s, execute %s, reconcile %s",
		formatPerfElapsed(snapshot.ObserveTimeMS),
		formatPerfElapsed(snapshot.PlanTimeMS),
		formatPerfElapsed(snapshot.ExecuteTimeMS),
		formatPerfElapsed(snapshot.ReconcileTimeMS),
	)
}

func formatDetailedPerfActivity(snapshot *perf.Snapshot, actionCount int) string {
	return fmt.Sprintf(
		"actions %d, watch batches %d, watch paths %d",
		actionCount,
		snapshot.WatchBatchCount,
		snapshot.WatchPathCount,
	)
}
