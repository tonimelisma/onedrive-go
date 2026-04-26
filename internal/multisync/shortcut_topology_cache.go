package multisync

import (
	"path"
	"reflect"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

func (o *Orchestrator) storeShortcutChildTopology(
	parentID mountID,
	snapshot syncengine.ShortcutChildTopologySnapshot,
) bool {
	if o == nil || parentID == "" {
		return false
	}
	snapshot.NamespaceID = parentID.String()
	o.shortcutMu.Lock()
	defer o.shortcutMu.Unlock()
	if o.shortcutChildren == nil {
		o.shortcutChildren = make(map[mountID]syncengine.ShortcutChildTopologySnapshot)
	}
	current, found := o.shortcutChildren[parentID]
	if found && reflect.DeepEqual(current, snapshot) {
		return false
	}
	o.shortcutChildren[parentID] = snapshot
	return true
}

func (o *Orchestrator) shortcutChildTopologyFor(parentID mountID) syncengine.ShortcutChildTopologySnapshot {
	if o == nil || parentID == "" {
		return syncengine.ShortcutChildTopologySnapshot{NamespaceID: parentID.String()}
	}
	o.shortcutMu.Lock()
	defer o.shortcutMu.Unlock()
	snapshot := o.shortcutChildren[parentID]
	snapshot.NamespaceID = parentID.String()
	snapshot.Children = append([]syncengine.ShortcutChildTopology(nil), snapshot.Children...)
	for i := range snapshot.Children {
		if snapshot.Children[i].Waiting != nil {
			waiting := *snapshot.Children[i].Waiting
			snapshot.Children[i].Waiting = &waiting
		}
		snapshot.Children[i].ProtectedPaths = append([]string(nil), snapshot.Children[i].ProtectedPaths...)
	}
	return snapshot
}

func (o *Orchestrator) transientShortcutTopology(parents []*mountSpec) *childMountTopology {
	topology := defaultChildMountTopology()
	for _, parent := range parents {
		if parent == nil || parent.projectionKind != MountProjectionStandalone {
			continue
		}
		snapshot := o.shortcutChildTopologyFor(parent.mountID)
		for i := range snapshot.Children {
			record, ok := childTopologyRecordFromTopology(parent, &snapshot.Children[i])
			if !ok {
				continue
			}
			topology.mounts[record.mountID] = record
		}
	}
	return topology
}

func childTopologyRecordFromTopology(
	parent *mountSpec,
	child *syncengine.ShortcutChildTopology,
) (childTopologyRecord, bool) {
	if parent == nil || child == nil || child.BindingItemID == "" {
		return childTopologyRecord{}, false
	}
	relativePath := child.RelativeLocalPath
	if relativePath == "" {
		return childTopologyRecord{}, false
	}
	localAlias := child.LocalAlias
	if localAlias == "" {
		localAlias = path.Base(relativePath)
	}
	record := childTopologyRecord{
		mountID:             config.ChildMountID(parent.mountID.String(), child.BindingItemID),
		namespaceID:         parent.mountID.String(),
		bindingItemID:       child.BindingItemID,
		localAlias:          localAlias,
		relativeLocalPath:   relativePath,
		reservedLocalPaths:  reservedPathsFromProtected(relativePath, child.ProtectedPaths),
		tokenOwnerCanonical: parent.tokenOwnerCanonical.String(),
		remoteDriveID:       child.RemoteDriveID,
		remoteItemID:        child.RemoteItemID,
	}
	switch child.State {
	case "", syncengine.ShortcutChildDesired:
		record.state = childTopologyStateActive
	case syncengine.ShortcutChildRetiring:
		record.state = childTopologyStatePendingRemoval
		record.stateReason = childTopologyStateReasonShortcutRemoved
	case syncengine.ShortcutChildBlocked:
		record.state = childTopologyStateUnavailable
		record.stateReason = childTopologyStateReasonShortcutBindingUnavailable
	case syncengine.ShortcutChildWaitingReplacement:
		record.state = childTopologyStateUnavailable
		record.stateReason = childTopologyStateReasonShortcutBindingUnavailable
	default:
		record.state = childTopologyStateUnavailable
		record.stateReason = childTopologyStateReasonShortcutBindingUnavailable
	}
	if record.remoteDriveID == "" || record.remoteItemID == "" {
		record.state = childTopologyStateUnavailable
		record.stateReason = childTopologyStateReasonShortcutBindingUnavailable
	}
	if _, err := driveid.NewCanonicalID(record.tokenOwnerCanonical); err != nil {
		return childTopologyRecord{}, false
	}
	return record, true
}

func reservedPathsFromProtected(current string, protected []string) []string {
	reserved := make([]string, 0, len(protected))
	seen := map[string]struct{}{current: {}}
	for _, protectedPath := range protected {
		if protectedPath == "" {
			continue
		}
		if _, ok := seen[protectedPath]; ok {
			continue
		}
		seen[protectedPath] = struct{}{}
		reserved = append(reserved, protectedPath)
	}
	return reserved
}
