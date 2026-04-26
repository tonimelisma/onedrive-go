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
	if found {
		publication.Released = mergeReleasedShortcutChildren(current, publication)
	}
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

func mergeReleasedShortcutChildren(
	current syncengine.ShortcutChildTopologyPublication,
	next syncengine.ShortcutChildTopologyPublication,
) []syncengine.ShortcutChildRelease {
	nextByBinding := make(map[string]struct{}, len(next.Children))
	for i := range next.Children {
		if next.Children[i].BindingItemID != "" {
			nextByBinding[next.Children[i].BindingItemID] = struct{}{}
		}
	}

	released := make([]syncengine.ShortcutChildRelease, 0, len(current.Released)+len(next.Released))
	releasedByBinding := make(map[string]struct{}, len(current.Released)+len(next.Released)+len(current.Children))
	appendRelease := func(release syncengine.ShortcutChildRelease) {
		if release.BindingItemID == "" {
			return
		}
		if _, resurrected := nextByBinding[release.BindingItemID]; resurrected {
			return
		}
		if _, alreadyReleased := releasedByBinding[release.BindingItemID]; alreadyReleased {
			return
		}
		if release.Reason == "" {
			release.Reason = syncengine.ShortcutChildReleaseParentRemoved
		}
		released = append(released, release)
		releasedByBinding[release.BindingItemID] = struct{}{}
	}

	for i := range current.Released {
		appendRelease(current.Released[i])
	}
	for i := range next.Released {
		appendRelease(next.Released[i])
	}
	for i := range current.Children {
		child := current.Children[i]
		if child.BindingItemID == "" {
			continue
		}
		if _, stillPresent := nextByBinding[child.BindingItemID]; stillPresent {
			continue
		}
		appendRelease(syncengine.ShortcutChildRelease{
			BindingItemID: child.BindingItemID,
			Reason:        syncengine.ShortcutChildReleaseParentRemoved,
		})
	}
	return released
}

func (o *Orchestrator) forgetReleasedShortcutChildren(releases []releasedShortcutChild) bool {
	if o == nil || len(releases) == 0 {
		return false
	}
	byNamespace := make(map[string]map[string]struct{})
	for i := range releases {
		release := releases[i]
		if release.namespaceID == "" || release.bindingItemID == "" {
			continue
		}
		bindings := byNamespace[release.namespaceID]
		if bindings == nil {
			bindings = make(map[string]struct{})
			byNamespace[release.namespaceID] = bindings
		}
		bindings[release.bindingItemID] = struct{}{}
	}
	if len(byNamespace) == 0 {
		return false
	}

	o.shortcutMu.Lock()
	defer o.shortcutMu.Unlock()
	changed := false
	for parentID, publication := range o.shortcutChildren {
		namespaceID := publication.NamespaceID
		if namespaceID == "" {
			namespaceID = parentID.String()
		}
		bindings := byNamespace[namespaceID]
		if len(bindings) == 0 || len(publication.Released) == 0 {
			continue
		}
		parentChanged := false
		filtered := make([]syncengine.ShortcutChildRelease, 0, len(publication.Released))
		for i := range publication.Released {
			if _, forget := bindings[publication.Released[i].BindingItemID]; forget {
				parentChanged = true
				continue
			}
			filtered = append(filtered, publication.Released[i])
		}
		if parentChanged {
			changed = true
			publication.Released = filtered
			o.shortcutChildren[parentID] = publication
		}
	}
	return changed
}

func cloneShortcutTopologyPublication(
	publication syncengine.ShortcutChildTopologyPublication,
) syncengine.ShortcutChildTopologyPublication {
	publication.Children = cloneShortcutChildren(publication.Children)
	publication.Released = append([]syncengine.ShortcutChildRelease(nil), publication.Released...)
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
