package cli

import (
	"context"
	"fmt"

	synccontrol "github.com/tonimelisma/onedrive-go/internal/synccontrol"
)

const (
	statusRuntimeStateActive   = "active"
	statusRuntimeStateInactive = "inactive"

	statusRuntimeUnansweredNote = "Note: the running sync did not answer in time, so its live state is not shown.\n"
)

type statusRuntimeOverlay struct {
	ownerMode    synccontrol.OwnerMode
	activeMounts map[string]struct{}
	// unavailableNote says why a sync that holds the control socket
	// contributed no live state; empty when there is nothing to say, or when
	// the user stopped status. Without it a drive the watch is syncing reads
	// as pending with no reason given.
	unavailableNote string
}

// loadStatusRuntimeOverlay reads the running sync's live state from probe,
// the one control-socket probe status makes (probeErr is its error).
func loadStatusRuntimeOverlay(ctx context.Context, probe controlOwnerProbe, probeErr error) statusRuntimeOverlay {
	switch probe.state {
	case controlOwnerStateWatchOwner, controlOwnerStateOneShotOwner:
		overlay := statusRuntimeOverlay{
			ownerMode:    probe.client.ownerMode(),
			activeMounts: make(map[string]struct{}, len(probe.client.status.Mounts)),
		}
		for _, mountID := range probe.client.status.Mounts {
			overlay.activeMounts[mountID] = struct{}{}
		}
		return overlay
	case controlOwnerStateUnresponsive, controlOwnerStateProbeFailed:
		if ctx.Err() != nil {
			// The user stopped status, so the request ended with the
			// command: that is no news about the running sync, and a stop
			// prints no warnings.
			return statusRuntimeOverlay{}
		}
		if probe.state == controlOwnerStateUnresponsive {
			return statusRuntimeOverlay{unavailableNote: statusRuntimeUnansweredNote}
		}

		return statusRuntimeOverlay{
			unavailableNote: fmt.Sprintf("Note: could not read the running sync's live state (%v), so it is not shown.\n", probeErr),
		}
	case controlOwnerStateNoSocket, controlOwnerStatePathUnavailable:
		return statusRuntimeOverlay{}
	}

	return statusRuntimeOverlay{}
}

func applyStatusRuntimeOverlay(accounts []statusAccount, overlay statusRuntimeOverlay) {
	if overlay.ownerMode == "" {
		return
	}

	for i := range accounts {
		for j := range accounts[i].Drives {
			applyStatusRuntimeOverlayToDrive(&accounts[i].Drives[j], overlay)
		}
	}
}

func applyStatusRuntimeOverlayToDrive(drive *statusDrive, overlay statusRuntimeOverlay) {
	if drive == nil {
		return
	}

	drive.RuntimeOwner = string(overlay.ownerMode)
	if _, ok := overlay.activeMounts[drive.InternalID]; ok {
		drive.RuntimeState = statusRuntimeStateActive
	} else {
		drive.RuntimeState = statusRuntimeStateInactive
	}
	finalizeStatusDriveState(drive)

	for i := range drive.SharedFolders {
		applyStatusRuntimeOverlayToDrive(&drive.SharedFolders[i], overlay)
	}
}
