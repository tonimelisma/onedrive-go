package multisync

import (
	"cmp"
	"reflect"
	"slices"

	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

func (o *Orchestrator) storeParentShortcutTopology(
	parentID mountID,
	publication syncengine.ShortcutChildTopologyPublication,
) bool {
	if o == nil || parentID == "" {
		return false
	}
	publication = normalizeShortcutTopologyPublication(parentID, publication)

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

func (o *Orchestrator) forgetParentShortcutTopology(parentID mountID) {
	if o == nil || parentID == "" {
		return
	}
	o.shortcutMu.Lock()
	defer o.shortcutMu.Unlock()
	delete(o.shortcutChildren, parentID)
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
	publication.Children = cloneShortcutChildren(publication.Children)
	publication.CleanupRequests = append(
		[]syncengine.ShortcutChildArtifactCleanupRequest(nil),
		publication.CleanupRequests...,
	)
	return publication
}

func normalizeShortcutTopologyPublication(
	parentID mountID,
	publication syncengine.ShortcutChildTopologyPublication,
) syncengine.ShortcutChildTopologyPublication {
	publication.NamespaceID = parentID.String()
	publication = cloneShortcutTopologyPublication(publication)
	if len(publication.Children) == 0 {
		publication.Children = nil
	}
	if len(publication.CleanupRequests) == 0 {
		publication.CleanupRequests = nil
	}
	for i := range publication.Children {
		if len(publication.Children[i].ProtectedPaths) == 0 {
			publication.Children[i].ProtectedPaths = nil
		} else {
			slices.Sort(publication.Children[i].ProtectedPaths)
		}
	}
	slices.SortFunc(publication.Children, func(a, b syncengine.ShortcutChildTopology) int {
		if byBinding := cmp.Compare(a.BindingItemID, b.BindingItemID); byBinding != 0 {
			return byBinding
		}
		return cmp.Compare(a.RelativeLocalPath, b.RelativeLocalPath)
	})
	slices.SortFunc(publication.CleanupRequests, func(a, b syncengine.ShortcutChildArtifactCleanupRequest) int {
		if byBinding := cmp.Compare(a.BindingItemID, b.BindingItemID); byBinding != 0 {
			return byBinding
		}
		if byPath := cmp.Compare(a.RelativeLocalPath, b.RelativeLocalPath); byPath != 0 {
			return byPath
		}
		return cmp.Compare(a.Reason, b.Reason)
	})
	return publication
}

func cloneShortcutChildren(children []syncengine.ShortcutChildTopology) []syncengine.ShortcutChildTopology {
	cloned := append([]syncengine.ShortcutChildTopology(nil), children...)
	for i := range cloned {
		cloned[i] = cloneShortcutChild(&cloned[i])
	}
	return cloned
}

func cloneShortcutChild(child *syncengine.ShortcutChildTopology) syncengine.ShortcutChildTopology {
	if child == nil {
		return syncengine.ShortcutChildTopology{}
	}
	cloned := *child
	cloned.ProtectedPaths = append([]string(nil), cloned.ProtectedPaths...)
	if cloned.LocalRootIdentity != nil {
		identity := *cloned.LocalRootIdentity
		cloned.LocalRootIdentity = &identity
	}
	if cloned.Waiting != nil {
		waiting := cloneShortcutChild(cloned.Waiting)
		cloned.Waiting = &waiting
	}
	return cloned
}

func cloneChildRootIdentity(identity *syncengine.ShortcutRootIdentity) *syncengine.ShortcutRootIdentity {
	if identity == nil {
		return nil
	}
	next := *identity
	return &next
}
