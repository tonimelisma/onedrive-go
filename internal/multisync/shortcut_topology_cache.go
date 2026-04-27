package multisync

import (
	"cmp"
	"reflect"
	"slices"

	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

func (o *Orchestrator) receiveParentRunnerPublication(
	parentID mountID,
	publication syncengine.ShortcutChildRunnerPublication,
) bool {
	if o == nil || parentID == "" {
		return false
	}
	publication = normalizeParentRunnerPublication(parentID, publication)

	o.shortcutMu.Lock()
	defer o.shortcutMu.Unlock()
	if o.latestParentRunnerPublications == nil {
		o.latestParentRunnerPublications = make(map[mountID]syncengine.ShortcutChildRunnerPublication)
	}
	current, found := o.latestParentRunnerPublications[parentID]
	if found && reflect.DeepEqual(current, publication) {
		return false
	}
	o.latestParentRunnerPublications[parentID] = publication
	return true
}

func (o *Orchestrator) latestParentRunnerPublicationFor(parentID mountID) syncengine.ShortcutChildRunnerPublication {
	if o == nil || parentID == "" {
		return syncengine.ShortcutChildRunnerPublication{NamespaceID: parentID.String()}
	}
	o.shortcutMu.Lock()
	defer o.shortcutMu.Unlock()
	publication := o.latestParentRunnerPublications[parentID]
	publication.NamespaceID = parentID.String()
	return cloneParentRunnerPublication(publication)
}

func (o *Orchestrator) forgetParentRunnerPublication(parentID mountID) {
	if o == nil || parentID == "" {
		return
	}
	o.shortcutMu.Lock()
	defer o.shortcutMu.Unlock()
	delete(o.latestParentRunnerPublications, parentID)
}

func (o *Orchestrator) latestParentRunnerPublicationsFor(parents []*mountSpec) map[mountID]syncengine.ShortcutChildRunnerPublication {
	publications := make(map[mountID]syncengine.ShortcutChildRunnerPublication)
	for _, parent := range parents {
		if parent == nil || parent.projectionKind != MountProjectionStandalone {
			continue
		}
		publications[parent.mountID] = o.latestParentRunnerPublicationFor(parent.mountID)
	}
	return publications
}

func cloneParentRunnerPublication(
	publication syncengine.ShortcutChildRunnerPublication,
) syncengine.ShortcutChildRunnerPublication {
	publication.Children = cloneShortcutChildRunners(publication.Children)
	publication.CleanupRequests = append(
		[]syncengine.ShortcutChildArtifactCleanupRequest(nil),
		publication.CleanupRequests...,
	)
	return publication
}

func normalizeParentRunnerPublication(
	parentID mountID,
	publication syncengine.ShortcutChildRunnerPublication,
) syncengine.ShortcutChildRunnerPublication {
	publication.NamespaceID = parentID.String()
	publication = cloneParentRunnerPublication(publication)
	if len(publication.Children) == 0 {
		publication.Children = nil
	}
	if len(publication.CleanupRequests) == 0 {
		publication.CleanupRequests = nil
	}
	slices.SortFunc(publication.Children, func(a, b syncengine.ShortcutChildRunner) int {
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

func cloneShortcutChildRunners(children []syncengine.ShortcutChildRunner) []syncengine.ShortcutChildRunner {
	cloned := append([]syncengine.ShortcutChildRunner(nil), children...)
	for i := range cloned {
		cloned[i] = cloneShortcutChildRunner(&cloned[i])
	}
	return cloned
}

func cloneShortcutChildRunner(child *syncengine.ShortcutChildRunner) syncengine.ShortcutChildRunner {
	if child == nil {
		return syncengine.ShortcutChildRunner{}
	}
	cloned := *child
	if cloned.LocalRootIdentity != nil {
		identity := *cloned.LocalRootIdentity
		cloned.LocalRootIdentity = &identity
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
