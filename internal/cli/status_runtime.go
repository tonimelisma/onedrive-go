package cli

import (
	"context"

	synccontrol "github.com/tonimelisma/onedrive-go/internal/synccontrol"
)

const (
	statusRuntimeStateActive   = "active"
	statusRuntimeStateInactive = "inactive"
)

type statusRuntimeOverlay struct {
	ownerMode    synccontrol.OwnerMode
	activeMounts map[string]struct{}
}

func loadStatusRuntimeOverlay(ctx context.Context) statusRuntimeOverlay {
	probe, err := probeControlOwner(ctx)
	if err != nil {
		return statusRuntimeOverlay{}
	}

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
	case controlOwnerStateNoSocket,
		controlOwnerStatePathUnavailable,
		controlOwnerStateProbeFailed:
		return statusRuntimeOverlay{}
	}

	return statusRuntimeOverlay{}
}

func applyStatusRuntimeOverlay(accounts []statusAccount, overlay statusRuntimeOverlay) {
	if overlay.ownerMode == "" {
		return
	}

	for i := range accounts {
		for j := range accounts[i].Mounts {
			applyStatusRuntimeOverlayToDrive(&accounts[i].Mounts[j], overlay)
		}
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
	internalID := drive.InternalID
	if internalID == "" {
		internalID = drive.MountID
	}
	if internalID == "" {
		internalID = drive.CanonicalID
	}
	if _, ok := overlay.activeMounts[internalID]; ok {
		drive.RuntimeState = statusRuntimeStateActive
	} else {
		drive.RuntimeState = statusRuntimeStateInactive
	}
	finalizeStatusDriveState(drive)

	for i := range drive.ChildMounts {
		applyStatusRuntimeOverlayToDrive(&drive.ChildMounts[i], overlay)
	}
	for i := range drive.SharedFolders {
		applyStatusRuntimeOverlayToDrive(&drive.SharedFolders[i], overlay)
	}
}
