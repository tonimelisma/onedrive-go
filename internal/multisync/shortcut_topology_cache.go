package multisync

import (
	"reflect"

	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

func (o *Orchestrator) storeParentShortcutTopology(
	parentID mountID,
	publication syncengine.ShortcutChildTopologyPublication,
) bool {
	if o == nil || parentID == "" {
		return false
	}
	publication.NamespaceID = parentID.String()
	publication = cloneShortcutTopologyPublication(publication)
	o.shortcutMu.Lock()
	defer o.shortcutMu.Unlock()
	if o.shortcutChildren == nil {
		o.shortcutChildren = make(map[mountID]syncengine.ShortcutChildTopologyPublication)
	}
	current, found := o.shortcutChildren[parentID]
	if found && reflect.DeepEqual(current, publication) {
		return false
	}
	o.shortcutChildren[parentID] = publication
	return true
}

func (o *Orchestrator) parentShortcutTopologyFor(parentID mountID) syncengine.ShortcutChildTopologyPublication {
	if o == nil || parentID == "" {
		return syncengine.ShortcutChildTopologyPublication{NamespaceID: parentID.String()}
	}
	o.shortcutMu.Lock()
	defer o.shortcutMu.Unlock()
	publication := o.shortcutChildren[parentID]
	publication.NamespaceID = parentID.String()
	return cloneShortcutTopologyPublication(publication)
}

func (o *Orchestrator) parentShortcutTopologiesFor(parents []*mountSpec) map[mountID]syncengine.ShortcutChildTopologyPublication {
	topologies := make(map[mountID]syncengine.ShortcutChildTopologyPublication)
	for _, parent := range parents {
		if parent == nil || parent.projectionKind != MountProjectionStandalone {
			continue
		}
		topologies[parent.mountID] = o.parentShortcutTopologyFor(parent.mountID)
	}
	return topologies
}

func cloneShortcutTopologyPublication(
	publication syncengine.ShortcutChildTopologyPublication,
) syncengine.ShortcutChildTopologyPublication {
	publication.Children = append([]syncengine.ShortcutChildTopology(nil), publication.Children...)
	for i := range publication.Children {
		if publication.Children[i].Waiting != nil {
			waiting := *publication.Children[i].Waiting
			waiting.ProtectedPaths = append([]string(nil), waiting.ProtectedPaths...)
			publication.Children[i].Waiting = &waiting
		}
		publication.Children[i].ProtectedPaths = append([]string(nil), publication.Children[i].ProtectedPaths...)
	}
	return publication
}
