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
			applyStatusRuntimeOverlayToMount(&accounts[i].Mounts[j], overlay)
		}
	}
}

func applyStatusRuntimeOverlayToMount(mount *statusMount, overlay statusRuntimeOverlay) {
	if mount == nil {
		return
	}

	mount.RuntimeOwner = string(overlay.ownerMode)
	if _, ok := overlay.activeMounts[mount.MountID]; ok {
		mount.RuntimeState = statusRuntimeStateActive
	} else {
		mount.RuntimeState = statusRuntimeStateInactive
	}

	for i := range mount.ChildMounts {
		applyStatusRuntimeOverlayToMount(&mount.ChildMounts[i], overlay)
	}
}
