package multisync

import (
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

func (o *Orchestrator) receiveParentRunnerPublication(
	parentID mountID,
	publication syncengine.ShortcutChildRunnerPublication,
) bool {
	if o == nil || parentID == "" {
		return false
	}
	publication = syncengine.NormalizeShortcutChildRunnerPublication(parentID.String(), publication)

	o.shortcutMu.Lock()
	defer o.shortcutMu.Unlock()
	if o.latestParentRunnerPublications == nil {
		o.latestParentRunnerPublications = make(map[mountID]syncengine.ShortcutChildRunnerPublication)
	}
	current, found := o.latestParentRunnerPublications[parentID]
	if found && syncengine.ShortcutChildRunnerPublicationsEqual(current, publication) {
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
	return syncengine.NormalizeShortcutChildRunnerPublication(parentID.String(), publication)
}

func (o *Orchestrator) parentRunnerPublicationHasOneShotWork(parentID mountID) bool {
	publication := o.latestParentRunnerPublicationFor(parentID)
	return len(publication.RunnerWork.Children) > 0 || len(publication.CleanupWork.Requests) > 0
}

func (o *Orchestrator) parentRunnerPublicationHasImmediateOneShotWork(parentID mountID) bool {
	publication := o.latestParentRunnerPublicationFor(parentID)
	if len(publication.CleanupWork.Requests) > 0 {
		return true
	}
	for i := range publication.RunnerWork.Children {
		child := &publication.RunnerWork.Children[i]
		switch child.RunnerAction {
		case syncengine.ShortcutChildActionRun,
			syncengine.ShortcutChildActionFinalDrain:
			return true
		case syncengine.ShortcutChildActionSkipParentBlocked:
		}
	}
	return false
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

func cloneChildRootIdentity(identity *syncengine.ShortcutRootIdentity) *syncengine.ShortcutRootIdentity {
	if identity == nil {
		return nil
	}
	next := *identity
	return &next
}
