package multisync

import (
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

func (o *Orchestrator) receiveParentChildWorkSnapshot(
	parentID mountID,
	snapshot syncengine.ShortcutChildWorkSnapshot,
) bool {
	if o == nil || parentID == "" {
		return false
	}
	snapshot = syncengine.NormalizeShortcutChildWorkSnapshot(parentID.String(), snapshot)

	o.shortcutMu.Lock()
	defer o.shortcutMu.Unlock()
	if o.latestParentChildWorkSnapshots == nil {
		o.latestParentChildWorkSnapshots = make(map[mountID]syncengine.ShortcutChildWorkSnapshot)
	}
	current, found := o.latestParentChildWorkSnapshots[parentID]
	if found && syncengine.ShortcutChildWorkSnapshotsEqual(current, snapshot) {
		return false
	}
	o.latestParentChildWorkSnapshots[parentID] = snapshot
	return true
}

func (o *Orchestrator) latestParentChildWorkSnapshotFor(parentID mountID) syncengine.ShortcutChildWorkSnapshot {
	if o == nil || parentID == "" {
		return syncengine.ShortcutChildWorkSnapshot{NamespaceID: parentID.String()}
	}
	o.shortcutMu.Lock()
	defer o.shortcutMu.Unlock()
	snapshot := o.latestParentChildWorkSnapshots[parentID]
	snapshot.NamespaceID = parentID.String()
	return syncengine.NormalizeShortcutChildWorkSnapshot(parentID.String(), snapshot)
}

func (o *Orchestrator) parentChildWorkSnapshotHasWork(parentID mountID) bool {
	snapshot := o.latestParentChildWorkSnapshotFor(parentID)
	return len(snapshot.RunCommands) > 0 || len(snapshot.CleanupCommands) > 0
}

func (o *Orchestrator) forgetParentChildWorkSnapshot(parentID mountID) {
	if o == nil || parentID == "" {
		return
	}
	o.shortcutMu.Lock()
	defer o.shortcutMu.Unlock()
	delete(o.latestParentChildWorkSnapshots, parentID)
}

func (o *Orchestrator) latestParentChildWorkSnapshotsFor(parents []*mountSpec) map[mountID]syncengine.ShortcutChildWorkSnapshot {
	snapshots := make(map[mountID]syncengine.ShortcutChildWorkSnapshot)
	for _, parent := range parents {
		if parent == nil || parent.projectionKind != MountProjectionStandalone {
			continue
		}
		snapshots[parent.mountID] = o.latestParentChildWorkSnapshotFor(parent.mountID)
	}
	return snapshots
}
