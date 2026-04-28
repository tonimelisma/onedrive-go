package sync

import (
	"path"
	"slices"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// ShortcutRootState is the parent-engine-owned lifecycle state for a shortcut
// alias root in the parent sync namespace. It is not child content retry state.
type ShortcutRootState string

const (
	ShortcutRootStateActive                     ShortcutRootState = "active"
	ShortcutRootStateTargetUnavailable          ShortcutRootState = "target_unavailable"
	ShortcutRootStateLocalRootUnavailable       ShortcutRootState = "local_root_unavailable"
	ShortcutRootStateBlockedPath                ShortcutRootState = "blocked_path"
	ShortcutRootStateRenameAmbiguous            ShortcutRootState = "rename_ambiguous"
	ShortcutRootStateAliasMutationBlocked       ShortcutRootState = "alias_mutation_blocked"
	ShortcutRootStateRemovedFinalDrain          ShortcutRootState = "removed_final_drain"
	ShortcutRootStateRemovedReleasePending      ShortcutRootState = "removed_release_pending"
	ShortcutRootStateRemovedCleanupBlocked      ShortcutRootState = "removed_cleanup_blocked"
	ShortcutRootStateRemovedChildCleanupPending ShortcutRootState = "removed_child_cleanup_pending"
	ShortcutRootStateSamePathReplacementWaiting ShortcutRootState = "same_path_replacement_waiting"
	ShortcutRootStateDuplicateTarget            ShortcutRootState = "duplicate_target"
)

// ShortcutRootRecord is the persisted parent namespace truth for one shortcut
// placeholder observed by the parent engine.
type ShortcutRootRecord struct {
	NamespaceID       string
	BindingItemID     string
	RelativeLocalPath string
	LocalAlias        string
	RemoteDriveID     driveid.ID
	RemoteItemID      string
	RemoteIsFolder    bool
	State             ShortcutRootState
	ProtectedPaths    []string
	BlockedDetail     string
	LocalRootIdentity *synctree.FileIdentity
	Waiting           *ShortcutRootReplacement
}

// ShortcutRootReplacement stores a new shortcut binding that is waiting behind
// an older retiring binding at the same parent-local path.
type ShortcutRootReplacement struct {
	BindingItemID     string
	RelativeLocalPath string
	LocalAlias        string
	RemoteDriveID     driveid.ID
	RemoteItemID      string
	RemoteIsFolder    bool
}

func protectedRootsForShortcutRoots(records []ShortcutRootRecord, namespaceID string) []ProtectedRoot {
	protectedRoots := make([]ProtectedRoot, 0)
	for i := range records {
		record := normalizeShortcutRootRecord(&records[i])
		if record.NamespaceID != "" && record.NamespaceID != namespaceID {
			continue
		}
		if !shortcutRootStateKeepsProtectedPaths(record.State) {
			continue
		}
		paths := protectedPathsForShortcutRoot(record.RelativeLocalPath, record.ProtectedPaths)
		for _, protectedPath := range paths {
			protectedRoot := ProtectedRoot{
				Path:           protectedPath,
				MountID:        record.BindingItemID,
				BindingID:      record.BindingItemID,
				RemoteDriveID:  record.RemoteDriveID,
				RemoteItemID:   record.RemoteItemID,
				RemoteIsFolder: record.RemoteIsFolder,
			}
			if record.LocalRootIdentity != nil {
				protectedRoot.Device = record.LocalRootIdentity.Device
				protectedRoot.Inode = record.LocalRootIdentity.Inode
				protectedRoot.HasIdentity = true
			}
			protectedRoots = append(protectedRoots, protectedRoot)
		}
	}
	return protectedRoots
}

func shortcutRootStateKeepsProtectedPaths(state ShortcutRootState) bool {
	metadata, ok := shortcutRootLifecycleMetadataFor(state)
	return ok && metadata.protectsPath
}

func normalizeShortcutRootRecord(record *ShortcutRootRecord) ShortcutRootRecord {
	if record == nil {
		return ShortcutRootRecord{State: ShortcutRootStateActive}
	}
	normalized := *record
	if normalized.LocalAlias == "" && normalized.RelativeLocalPath != "" {
		normalized.LocalAlias = path.Base(normalized.RelativeLocalPath)
	}
	normalized.ProtectedPaths = protectedPathsForShortcutRoot(normalized.RelativeLocalPath, normalized.ProtectedPaths)
	if normalized.State == "" {
		normalized.State = ShortcutRootStateActive
	}
	if normalized.State == ShortcutRootStateRemovedChildCleanupPending {
		normalized.ProtectedPaths = nil
		normalized.LocalRootIdentity = nil
		normalized.Waiting = nil
		return normalized
	}
	if normalized.Waiting != nil && normalized.Waiting.BindingItemID == "" {
		normalized.Waiting = nil
	}
	return normalized
}

func protectedPathsForShortcutRoot(relativeLocalPath string, paths []string) []string {
	unique := make(map[string]struct{}, len(paths)+1)
	result := make([]string, 0, len(paths)+1)
	add := func(value string) {
		normalized := normalizedProtectedRootPath(value)
		if normalized == "" {
			return
		}
		if _, exists := unique[normalized]; exists {
			return
		}
		unique[normalized] = struct{}{}
		result = append(result, normalized)
	}
	add(relativeLocalPath)
	for _, protectedPath := range paths {
		add(protectedPath)
	}
	return result
}

func shortcutRootRecordsEqual(a, b *ShortcutRootRecord) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	normalizedA := normalizeShortcutRootRecord(a)
	normalizedB := normalizeShortcutRootRecord(b)
	if normalizedA.NamespaceID != normalizedB.NamespaceID ||
		normalizedA.BindingItemID != normalizedB.BindingItemID ||
		normalizedA.RelativeLocalPath != normalizedB.RelativeLocalPath ||
		normalizedA.LocalAlias != normalizedB.LocalAlias ||
		normalizedA.RemoteDriveID.String() != normalizedB.RemoteDriveID.String() ||
		normalizedA.RemoteItemID != normalizedB.RemoteItemID ||
		normalizedA.RemoteIsFolder != normalizedB.RemoteIsFolder ||
		normalizedA.State != normalizedB.State ||
		normalizedA.BlockedDetail != normalizedB.BlockedDetail ||
		!slices.Equal(normalizedA.ProtectedPaths, normalizedB.ProtectedPaths) {
		return false
	}
	if !shortcutRootIdentitiesEqual(normalizedA.LocalRootIdentity, normalizedB.LocalRootIdentity) {
		return false
	}
	return shortcutRootReplacementsEqual(normalizedA.Waiting, normalizedB.Waiting)
}

func shortcutRootIdentitiesEqual(a, b *synctree.FileIdentity) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return synctree.SameIdentity(*a, *b)
}

func shortcutRootReplacementsEqual(a, b *ShortcutRootReplacement) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.BindingItemID == b.BindingItemID &&
		a.RelativeLocalPath == b.RelativeLocalPath &&
		a.LocalAlias == b.LocalAlias &&
		a.RemoteDriveID.String() == b.RemoteDriveID.String() &&
		a.RemoteItemID == b.RemoteItemID &&
		a.RemoteIsFolder == b.RemoteIsFolder
}

func cloneShortcutRootReplacement(replacement *ShortcutRootReplacement) *ShortcutRootReplacement {
	if replacement == nil {
		return nil
	}
	next := *replacement
	return &next
}

func cloneFileIdentity(identity *synctree.FileIdentity) *synctree.FileIdentity {
	if identity == nil {
		return nil
	}
	next := *identity
	return &next
}
