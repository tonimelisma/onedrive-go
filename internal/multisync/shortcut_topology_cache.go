package multisync

import (
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

func (o *Orchestrator) receiveParentChildProcessSnapshot(
	parentID mountID,
	snapshot syncengine.ShortcutChildProcessSnapshot,
) bool {
	if o == nil || parentID == "" {
		return false
	}
	snapshot = syncengine.NormalizeShortcutChildProcessSnapshot(parentID.String(), snapshot)

	o.shortcutMu.Lock()
	defer o.shortcutMu.Unlock()
	if o.latestParentChildProcessSnapshots == nil {
		o.latestParentChildProcessSnapshots = make(map[mountID]syncengine.ShortcutChildProcessSnapshot)
	}
	current, found := o.latestParentChildProcessSnapshots[parentID]
	if found && syncengine.ShortcutChildProcessSnapshotsEqual(current, snapshot) {
		return false
	}
	o.latestParentChildProcessSnapshots[parentID] = snapshot
	return true
}

func (o *Orchestrator) latestParentChildProcessSnapshotFor(parentID mountID) syncengine.ShortcutChildProcessSnapshot {
	if o == nil || parentID == "" {
		return syncengine.ShortcutChildProcessSnapshot{NamespaceID: parentID.String()}
	}
	o.shortcutMu.Lock()
	defer o.shortcutMu.Unlock()
	snapshot := o.latestParentChildProcessSnapshots[parentID]
	snapshot.NamespaceID = parentID.String()
	return syncengine.NormalizeShortcutChildProcessSnapshot(parentID.String(), snapshot)
}

func (o *Orchestrator) parentChildProcessSnapshotHasWork(parentID mountID) bool {
	snapshot := o.latestParentChildProcessSnapshotFor(parentID)
	return len(snapshot.RunCommands) > 0 || len(snapshot.Cleanups) > 0
}

func (o *Orchestrator) forgetParentChildProcessSnapshot(parentID mountID) {
	if o == nil || parentID == "" {
		return
	}
	o.shortcutMu.Lock()
	defer o.shortcutMu.Unlock()
	delete(o.latestParentChildProcessSnapshots, parentID)
}

func (o *Orchestrator) latestParentChildProcessSnapshotsFor(parents []*mountSpec) map[mountID]syncengine.ShortcutChildProcessSnapshot {
	snapshots := make(map[mountID]syncengine.ShortcutChildProcessSnapshot)
	for _, parent := range parents {
		if parent == nil || parent.projectionKind != MountProjectionStandalone {
			continue
		}
		snapshots[parent.mountID] = o.latestParentChildProcessSnapshotFor(parent.mountID)
	}
	return snapshots
}
