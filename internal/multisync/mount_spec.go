package multisync

import (
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

type mountID string

func (id mountID) String() string {
	return string(id)
}

type mountProjectionKind string

const (
	mountProjectionStandalone mountProjectionKind = "standalone"
	mountProjectionChild      mountProjectionKind = "child"
)

// resolvedDriveWithSelection keeps config-owned drive resolution separate from
// the control plane's runtime mount identity while preserving stable selection
// ordering for reporting and reload.
type resolvedDriveWithSelection struct {
	SelectionIndex int
	Drive          *config.ResolvedDrive
}

// mountSpec is the control plane's runtime unit. In this increment configured
// drives are the only source of mounts, so the spec still carries the resolved
// drive as a temporary adapter for drive-shaped session creation and the sync
// tunables that have not yet been promoted into mount-owned runtime state.
type mountSpec struct {
	mountID                   mountID
	projectionKind            mountProjectionKind
	selectionIndex            int
	canonicalID               driveid.CanonicalID
	displayName               string
	syncRoot                  string
	statePath                 string
	remoteDriveID             driveid.ID
	remoteRootItemID          string
	accountEmail              string
	paused                    bool
	enableWebsocket           bool
	rootedSubtreeDeltaCapable bool
	resolved                  *config.ResolvedDrive
}

func buildConfiguredMountSpecs(selected []*resolvedDriveWithSelection) ([]*mountSpec, error) {
	mounts := make([]*mountSpec, 0, len(selected))
	for i := range selected {
		mount, err := buildConfiguredMountSpec(selected[i])
		if err != nil {
			return nil, err
		}
		mounts = append(mounts, mount)
	}

	return mounts, nil
}

func buildConfiguredMountSpec(selected *resolvedDriveWithSelection) (*mountSpec, error) {
	if selected == nil || selected.Drive == nil {
		return nil, fmt.Errorf("multisync: resolved drive is required")
	}

	rd := selected.Drive
	statePath := rd.StatePath()
	if statePath == "" {
		return nil, fmt.Errorf("multisync: state path is required for %s", rd.CanonicalID)
	}

	return &mountSpec{
		mountID:                   mountID(rd.CanonicalID.String()),
		projectionKind:            mountProjectionStandalone,
		selectionIndex:            selected.SelectionIndex,
		canonicalID:               rd.CanonicalID,
		displayName:               rd.DisplayName,
		syncRoot:                  rd.SyncDir,
		statePath:                 statePath,
		remoteDriveID:             rd.DriveID,
		remoteRootItemID:          rd.RootItemID,
		accountEmail:              rd.CanonicalID.Email(),
		paused:                    rd.Paused,
		enableWebsocket:           rd.Websocket,
		rootedSubtreeDeltaCapable: rd.SharedRootDeltaCapable,
		resolved:                  rd,
	}, nil
}

func resolvedDrivesWithSelection(drives []*config.ResolvedDrive) []*resolvedDriveWithSelection {
	selected := make([]*resolvedDriveWithSelection, 0, len(drives))
	for i := range drives {
		selected = append(selected, &resolvedDriveWithSelection{
			SelectionIndex: i,
			Drive:          drives[i],
		})
	}

	return selected
}

func mountCanonicalIDs(mounts []*mountSpec) []string {
	ids := make([]string, 0, len(mounts))
	for i := range mounts {
		ids = append(ids, mounts[i].canonicalID.String())
	}

	return ids
}
