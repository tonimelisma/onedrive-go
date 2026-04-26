package sync

import (
	"context"
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

type ShortcutChildTopologySink func(context.Context, ShortcutChildTopologyPublication) error

type ShortcutChildRunnerAction string

const (
	ShortcutChildActionRun                    ShortcutChildRunnerAction = "run"
	ShortcutChildActionFinalDrain             ShortcutChildRunnerAction = "final_drain"
	ShortcutChildActionSkipParentBlocked      ShortcutChildRunnerAction = "skip_parent_blocked"
	ShortcutChildActionSkipWaitingReplacement ShortcutChildRunnerAction = "skip_waiting_replacement"
)

// ShortcutChildTopologyPublication is the parent engine's only shortcut
// lifecycle output to multisync. Raw shortcut observations stay inside
// internal/sync; multisync only sees the already-persisted child runner view.
type ShortcutChildTopologyPublication struct {
	NamespaceID     string
	Children        []ShortcutChildTopology
	CleanupRequests []ShortcutChildArtifactCleanupRequest
}

type ShortcutChildTopologySnapshot = ShortcutChildTopologyPublication

type ShortcutChildTopology struct {
	BindingItemID     string
	RelativeLocalPath string
	LocalAlias        string
	RemoteDriveID     string
	RemoteItemID      string
	RemoteIsFolder    bool
	RunnerAction      ShortcutChildRunnerAction
	BlockedDetail     string
	ProtectedPaths    []string
	LocalRootIdentity *ShortcutRootIdentity
	Waiting           *ShortcutChildTopology
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
	BindingItemID string
	Reason        ShortcutChildArtifactCleanupReason
}

type ShortcutChildDrainAck struct {
	BindingItemID string
}

type ShortcutChildArtifactCleanupAck struct {
	BindingItemID string
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
