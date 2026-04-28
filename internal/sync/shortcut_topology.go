package sync

import (
	"cmp"
	"context"
	"fmt"
	"path"
	"slices"

	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

type shortcutTopologyObservationKind string

const (
	shortcutTopologyObservationIncremental shortcutTopologyObservationKind = "incremental"
	shortcutTopologyObservationComplete    shortcutTopologyObservationKind = "complete"
)

type shortcutTopologyBatch struct {
	NamespaceID string
	Kind        shortcutTopologyObservationKind
	Upserts     []shortcutBindingUpsert
	Deletes     []shortcutBindingDelete
	Unavailable []shortcutBindingUnavailable
}

type shortcutBindingUpsert struct {
	BindingItemID     string
	RelativeLocalPath string
	LocalAlias        string
	RemoteDriveID     string
	RemoteItemID      string
	RemoteIsFolder    bool
	Complete          bool
}

type shortcutBindingDelete struct {
	BindingItemID string
}

type shortcutBindingUnavailable struct {
	BindingItemID     string
	RelativeLocalPath string
	LocalAlias        string
	RemoteDriveID     string
	RemoteItemID      string
	RemoteIsFolder    bool
	Reason            string
}

type ShortcutChildRunnerSink func(context.Context, ShortcutChildRunnerPublication) error

type ShortcutChildRunnerAction string

const (
	ShortcutChildActionRun               ShortcutChildRunnerAction = "run"
	ShortcutChildActionFinalDrain        ShortcutChildRunnerAction = "final_drain"
	ShortcutChildActionSkipParentBlocked ShortcutChildRunnerAction = "skip_parent_blocked"
)

// ShortcutChildRunnerPublication is the parent engine's only shortcut
// runner output to multisync. Raw shortcut observations and parent-visible
// status facts stay inside internal/sync; multisync only sees executable child
// runner work plus artifact cleanup requests.
type ShortcutChildRunnerPublication struct {
	NamespaceID string
	RunnerWork  ShortcutChildRunnerWork
	CleanupWork ShortcutChildArtifactCleanupWork
}

type ShortcutChildRunnerWork struct {
	Children []ShortcutChildRunner
}

type ShortcutChildArtifactCleanupWork struct {
	Requests []ShortcutChildArtifactCleanupRequest
}

type ShortcutChildRunner struct {
	ChildMountID      string
	BindingItemID     string
	RelativeLocalPath string
	LocalRoot         string
	DisplayName       string
	RemoteDriveID     string
	RemoteItemID      string
	RunnerAction      ShortcutChildRunnerAction
	LocalRootIdentity *ShortcutRootIdentity
}

// ShortcutChildAckHandle is the live-parent acknowledgement capability that
// multisync receives from a running parent engine. It is intentionally a value
// handle instead of an interface so control-plane code can invoke acknowledgements
// without owning or re-opening parent shortcut lifecycle state.
type ShortcutChildAckHandle struct {
	ackFinalDrain      func(context.Context, ShortcutChildDrainAck) (ShortcutChildRunnerPublication, error)
	ackArtifactsPurged func(context.Context, ShortcutChildArtifactCleanupAck) (ShortcutChildRunnerPublication, error)
}

func newShortcutChildAckHandle(
	ackFinalDrain func(context.Context, ShortcutChildDrainAck) (ShortcutChildRunnerPublication, error),
	ackArtifactsPurged func(context.Context, ShortcutChildArtifactCleanupAck) (ShortcutChildRunnerPublication, error),
) ShortcutChildAckHandle {
	return ShortcutChildAckHandle{
		ackFinalDrain:      ackFinalDrain,
		ackArtifactsPurged: ackArtifactsPurged,
	}
}

func (h ShortcutChildAckHandle) IsZero() bool {
	return h.ackFinalDrain == nil && h.ackArtifactsPurged == nil
}

func (h ShortcutChildAckHandle) AcknowledgeChildFinalDrain(
	ctx context.Context,
	ack ShortcutChildDrainAck,
) (ShortcutChildRunnerPublication, error) {
	if h.ackFinalDrain == nil {
		return ShortcutChildRunnerPublication{}, fmt.Errorf("sync: shortcut child final-drain ack requires live parent")
	}
	return h.ackFinalDrain(ctx, ack)
}

func (h ShortcutChildAckHandle) AcknowledgeChildArtifactsPurged(
	ctx context.Context,
	ack ShortcutChildArtifactCleanupAck,
) (ShortcutChildRunnerPublication, error) {
	if h.ackArtifactsPurged == nil {
		return ShortcutChildRunnerPublication{}, fmt.Errorf("sync: shortcut child artifact cleanup ack requires live parent")
	}
	return h.ackArtifactsPurged(ctx, ack)
}

// ShortcutRootIdentity is the parent-engine-issued local directory identity
// token for a managed shortcut root. Control-plane code may compare this value,
// but only sync opens the filesystem and decides whether it still matches.
type ShortcutRootIdentity struct {
	Device uint64
	Inode  uint64
}

func SameShortcutRootIdentity(a ShortcutRootIdentity, b ShortcutRootIdentity) bool {
	return a.Device == b.Device && a.Inode == b.Inode
}

func shortcutRootIdentityFromFileIdentity(identity *synctree.FileIdentity) *ShortcutRootIdentity {
	if identity == nil {
		return nil
	}
	return &ShortcutRootIdentity{
		Device: identity.Device,
		Inode:  identity.Inode,
	}
}

func shortcutRootIdentityToFileIdentity(identity *ShortcutRootIdentity) *synctree.FileIdentity {
	if identity == nil {
		return nil
	}
	return &synctree.FileIdentity{
		Device: identity.Device,
		Inode:  identity.Inode,
	}
}

type ShortcutChildArtifactCleanupReason string

const (
	ShortcutChildArtifactCleanupParentRemoved ShortcutChildArtifactCleanupReason = "parent_removed"
)

type ShortcutChildArtifactCleanupRequest struct {
	BindingItemID     string
	RelativeLocalPath string
	ChildMountID      string
	LocalRoot         string
	Reason            ShortcutChildArtifactCleanupReason
}

type ShortcutChildDrainAck struct {
	BindingItemID string
}

type ShortcutChildArtifactCleanupAck struct {
	BindingItemID string
}

func NormalizeShortcutChildRunnerPublication(
	namespaceID string,
	publication ShortcutChildRunnerPublication,
) ShortcutChildRunnerPublication {
	if publication.NamespaceID == "" {
		publication.NamespaceID = namespaceID
	}
	publication.RunnerWork.Children = cloneShortcutChildRunnerPublicationChildren(publication.RunnerWork.Children)
	publication.CleanupWork.Requests = append(
		[]ShortcutChildArtifactCleanupRequest(nil),
		publication.CleanupWork.Requests...,
	)
	if len(publication.RunnerWork.Children) == 0 {
		publication.RunnerWork.Children = nil
	}
	if len(publication.CleanupWork.Requests) == 0 {
		publication.CleanupWork.Requests = nil
	}
	slices.SortFunc(publication.RunnerWork.Children, func(a, b ShortcutChildRunner) int {
		if byBinding := cmp.Compare(a.BindingItemID, b.BindingItemID); byBinding != 0 {
			return byBinding
		}
		return cmp.Compare(a.RelativeLocalPath, b.RelativeLocalPath)
	})
	slices.SortFunc(publication.CleanupWork.Requests, func(a, b ShortcutChildArtifactCleanupRequest) int {
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

func ShortcutChildRunnerPublicationsEqual(
	a ShortcutChildRunnerPublication,
	b ShortcutChildRunnerPublication,
) bool {
	if a.NamespaceID != b.NamespaceID ||
		len(a.RunnerWork.Children) != len(b.RunnerWork.Children) ||
		len(a.CleanupWork.Requests) != len(b.CleanupWork.Requests) {
		return false
	}
	for i := range a.RunnerWork.Children {
		if !shortcutChildRunnerEqual(&a.RunnerWork.Children[i], &b.RunnerWork.Children[i]) {
			return false
		}
	}
	for i := range a.CleanupWork.Requests {
		if a.CleanupWork.Requests[i] != b.CleanupWork.Requests[i] {
			return false
		}
	}
	return true
}

func cloneShortcutChildRunnerPublicationChildren(children []ShortcutChildRunner) []ShortcutChildRunner {
	cloned := append([]ShortcutChildRunner(nil), children...)
	for i := range cloned {
		if cloned[i].LocalRootIdentity != nil {
			identity := *cloned[i].LocalRootIdentity
			cloned[i].LocalRootIdentity = &identity
		}
	}
	return cloned
}

func shortcutChildRunnerEqual(a *ShortcutChildRunner, b *ShortcutChildRunner) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if shortcutChildRunnerComparableFor(a) != shortcutChildRunnerComparableFor(b) {
		return false
	}
	return shortcutRootIdentityPointersEqual(a.LocalRootIdentity, b.LocalRootIdentity)
}

type shortcutChildRunnerComparable struct {
	ChildMountID      string
	BindingItemID     string
	RelativeLocalPath string
	LocalRoot         string
	DisplayName       string
	RemoteDriveID     string
	RemoteItemID      string
	RunnerAction      ShortcutChildRunnerAction
}

func shortcutChildRunnerComparableFor(child *ShortcutChildRunner) shortcutChildRunnerComparable {
	if child == nil {
		return shortcutChildRunnerComparable{}
	}
	return shortcutChildRunnerComparable{
		ChildMountID:      child.ChildMountID,
		BindingItemID:     child.BindingItemID,
		RelativeLocalPath: child.RelativeLocalPath,
		LocalRoot:         child.LocalRoot,
		DisplayName:       child.DisplayName,
		RemoteDriveID:     child.RemoteDriveID,
		RemoteItemID:      child.RemoteItemID,
		RunnerAction:      child.RunnerAction,
	}
}

func shortcutRootIdentityPointersEqual(a *ShortcutRootIdentity, b *ShortcutRootIdentity) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return SameShortcutRootIdentity(*a, *b)
	}
}

func (b shortcutTopologyBatch) hasFacts() bool {
	return len(b.Upserts) > 0 || len(b.Deletes) > 0 || len(b.Unavailable) > 0
}

func (b shortcutTopologyBatch) shouldApply() bool {
	return b.hasFacts() || b.Kind == shortcutTopologyObservationComplete
}

func (b *shortcutTopologyBatch) appendUpsert(fact shortcutBindingUpsert) {
	if fact.BindingItemID == "" {
		return
	}
	for i := range b.Upserts {
		if b.Upserts[i].BindingItemID == fact.BindingItemID {
			b.Upserts[i] = fact
			return
		}
	}
	b.removeUnavailable(fact.BindingItemID)
	b.removeDelete(fact.BindingItemID)
	b.Upserts = append(b.Upserts, fact)
}

func (b *shortcutTopologyBatch) appendDelete(fact shortcutBindingDelete) {
	if fact.BindingItemID == "" {
		return
	}
	b.removeUpsert(fact.BindingItemID)
	b.removeUnavailable(fact.BindingItemID)
	if slices.ContainsFunc(b.Deletes, func(existing shortcutBindingDelete) bool {
		return existing.BindingItemID == fact.BindingItemID
	}) {
		return
	}
	b.Deletes = append(b.Deletes, fact)
}

func (b *shortcutTopologyBatch) appendUnavailable(fact shortcutBindingUnavailable) {
	if fact.BindingItemID == "" {
		return
	}
	for i := range b.Unavailable {
		if b.Unavailable[i].BindingItemID == fact.BindingItemID {
			b.Unavailable[i] = fact
			return
		}
	}
	b.removeUpsert(fact.BindingItemID)
	b.removeDelete(fact.BindingItemID)
	b.Unavailable = append(b.Unavailable, fact)
}

func (b *shortcutTopologyBatch) removeUpsert(bindingItemID string) {
	b.Upserts = slices.DeleteFunc(b.Upserts, func(existing shortcutBindingUpsert) bool {
		return existing.BindingItemID == bindingItemID
	})
}

func (b *shortcutTopologyBatch) removeDelete(bindingItemID string) {
	b.Deletes = slices.DeleteFunc(b.Deletes, func(existing shortcutBindingDelete) bool {
		return existing.BindingItemID == bindingItemID
	})
}

func (b *shortcutTopologyBatch) removeUnavailable(bindingItemID string) {
	b.Unavailable = slices.DeleteFunc(b.Unavailable, func(existing shortcutBindingUnavailable) bool {
		return existing.BindingItemID == bindingItemID
	})
}

func protectedRootByBinding(protectedRoots []ProtectedRoot) map[string]ProtectedRoot {
	byBinding := make(map[string]ProtectedRoot, len(protectedRoots))
	for i := range protectedRoots {
		protectedRoot := protectedRoots[i]
		if protectedRoot.BindingID == "" {
			continue
		}
		byBinding[protectedRoot.BindingID] = protectedRoot
	}
	return byBinding
}

func protectedRootPrimaryName(relPath string) string {
	if relPath == "" {
		return ""
	}
	return path.Base(relPath)
}
