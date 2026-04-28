package multisync

import (
	gosync "sync"

	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

// parentChildWorkSnapshots owns exact parent publications for one runtime. It
// is deliberately not stored on Orchestrator so one-shot and watch runs cannot
// inherit shortcut child work from an earlier runtime.
type parentChildWorkSnapshots struct {
	mu        gosync.Mutex
	snapshots map[mountID]syncengine.ShortcutChildWorkSnapshot
}

func newParentChildWorkSnapshots() *parentChildWorkSnapshots {
	return &parentChildWorkSnapshots{
		snapshots: make(map[mountID]syncengine.ShortcutChildWorkSnapshot),
	}
}

func (c *parentChildWorkSnapshots) receive(
	parentID mountID,
	snapshot syncengine.ShortcutChildWorkSnapshot,
) (syncengine.ShortcutChildWorkSnapshot, bool) {
	snapshot = syncengine.NormalizeShortcutChildWorkSnapshot(parentID.String(), snapshot)
	if c == nil || parentID == "" {
		return snapshot, false
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.snapshots == nil {
		c.snapshots = make(map[mountID]syncengine.ShortcutChildWorkSnapshot)
	}
	current, found := c.snapshots[parentID]
	if found && syncengine.ShortcutChildWorkSnapshotsEqual(current, snapshot) {
		return snapshot, false
	}
	c.snapshots[parentID] = snapshot
	return snapshot, true
}

func (c *parentChildWorkSnapshots) latestFor(parentID mountID) syncengine.ShortcutChildWorkSnapshot {
	if c == nil || parentID == "" {
		return syncengine.NormalizeShortcutChildWorkSnapshot(parentID.String(), syncengine.ShortcutChildWorkSnapshot{})
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	snapshot := c.snapshots[parentID]
	snapshot.NamespaceID = parentID.String()
	return syncengine.NormalizeShortcutChildWorkSnapshot(parentID.String(), snapshot)
}

func shortcutChildWorkSnapshotHasWork(snapshot syncengine.ShortcutChildWorkSnapshot) bool {
	return len(snapshot.RunCommands) > 0 || len(snapshot.CleanupCommands) > 0
}

func (c *parentChildWorkSnapshots) forget(parentID mountID) {
	if c == nil || parentID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.snapshots, parentID)
}

func (c *parentChildWorkSnapshots) forParents(parents []*mountSpec) map[mountID]syncengine.ShortcutChildWorkSnapshot {
	snapshots := make(map[mountID]syncengine.ShortcutChildWorkSnapshot)
	for _, parent := range parents {
		if parent == nil || parent.projectionKind() != MountProjectionStandalone {
			continue
		}
		snapshots[parent.id()] = c.latestFor(parent.id())
	}
	return snapshots
}
