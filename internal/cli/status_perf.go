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
	statusPerfUnavailableInactive   = "mount is not active in the current sync owner; check logs"
	statusPerfUnavailableCollecting = "collecting live perf data; check logs"
)

type statusPerfOverlay struct {
	enabled           bool
	ownerPresent      bool
	unavailableReason string
	managedMounts     map[string]struct{}
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
			managedMounts: make(map[string]struct{}, len(probe.client.status.Mounts)),
		}
		for _, canonicalID := range probe.client.status.Mounts {
			overlay.managedMounts[canonicalID] = struct{}{}
		}

		response, perfErr := probe.client.perfStatus(ctx)
		if perfErr != nil {
			overlay.unavailableReason = statusPerfUnavailableGeneric
			return overlay
		}

		overlay.snapshots = response.Mounts
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
		for j := range accounts[i].Mounts {
			applyStatusPerfOverlayToMount(&accounts[i].Mounts[j], overlay)
		}
	}
}

func applyStatusPerfOverlayToMount(mount *statusMount, overlay statusPerfOverlay) {
	if mount == nil {
		return
	}

	mount.SyncState = overlaySyncState(mount.MountID, mount.SyncState, overlay)
	for i := range mount.ChildMounts {
		applyStatusPerfOverlayToMount(&mount.ChildMounts[i], overlay)
	}
}

func overlaySyncState(
	mountID string,
	state *syncStateInfo,
	overlay statusPerfOverlay,
) *syncStateInfo {
	snapshot, unavailableReason := overlay.lookup(mountID)
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

func (overlay statusPerfOverlay) lookup(mountID string) (*perf.Snapshot, string) {
	if !overlay.enabled {
		return nil, ""
	}

	if snapshot, ok := overlay.snapshots[mountID]; ok {
		snapshotCopy := snapshot
		return &snapshotCopy, ""
	}

	if !overlay.ownerPresent {
		return nil, overlay.unavailableReason
	}

	if _, ok := overlay.managedMounts[mountID]; !ok {
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

	return ss.hasPersistentSummaryData()
}

func (ss *syncStateInfo) hasPersistentSummaryData() bool {
	return ss.FileCount > 0 ||
		ss.ConditionCount > 0 ||
		len(ss.Conditions) > 0 ||
		ss.RemoteDrift > 0 ||
		ss.Retrying > 0
}

func printStatusPerfText(w io.Writer, indent string, ss *syncStateInfo) error {
	if ss == nil || (ss.Perf == nil && ss.PerfUnavailableReason == "") {
		return nil
	}

	if err := writeln(w); err != nil {
		return err
	}
	if err := writeln(w, indent+"PERF"); err != nil {
		return err
	}

	if ss.Perf == nil {
		return writef(w, "%sLive performance unavailable: %s\n", indent, ss.PerfUnavailableReason)
	}

	snapshot := ss.Perf
	actionCount := snapshot.ExecuteActionCount
	if actionCount == 0 {
		actionCount = snapshot.ActionableActionCount
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
		if err := writef(w, "%s%-10s %s\n", indent, line.label+":", line.value); err != nil {
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
		"observe %s, plan %s, execute %s, refresh %s",
		formatPerfElapsed(snapshot.ObserveTimeMS),
		formatPerfElapsed(snapshot.PlanTimeMS),
		formatPerfElapsed(snapshot.ExecuteTimeMS),
		formatPerfElapsed(snapshot.RefreshTimeMS),
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
