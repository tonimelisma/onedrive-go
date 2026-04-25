package multisync

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

const childMountRootDirPerms os.FileMode = 0o700

type childRootLifecycleActionKind string

const (
	childRootLifecycleActionRename childRootLifecycleActionKind = "rename"
	childRootLifecycleActionDelete childRootLifecycleActionKind = "delete"
)

type childRootLifecycleAction struct {
	kind                  childRootLifecycleActionKind
	mountID               mountID
	parent                *mountSpec
	selectionIndex        int
	identity              MountIdentity
	displayName           string
	bindingItemID         string
	fromRelativeLocalPath string
	toRelativeLocalPath   string
}

type childRootLifecycleDecision struct {
	state                 config.MountState
	reason                string
	identity              *config.RootIdentity
	localRootMaterialized bool
	action                *childRootLifecycleAction
	reservedLocalPaths    []string
}

func reconcileChildMountLocalRoots(
	parents []*mountSpec,
	inventory *config.MountInventory,
	logger *slog.Logger,
) mountInventoryMutationResult {
	if inventory == nil || len(inventory.Mounts) == 0 {
		return mountInventoryMutationResult{}
	}

	parentsByID := make(map[string]*mountSpec, len(parents))
	for i := range parents {
		if parents[i] == nil {
			continue
		}
		parentsByID[parents[i].mountID.String()] = parents[i]
	}

	result := mountInventoryMutationResult{}
	records := sortedMountRecords(inventory)
	for i := range records {
		record := records[i]
		if !childRootNeedsReconciliation(&record) {
			continue
		}
		parent := parentsByID[record.NamespaceID]
		if parent == nil {
			continue
		}

		changed, action := reconcileChildMountRootRecord(inventory, &record, parent, logger)
		if changed {
			result.changed = true
			result.dirtyMountIDs = appendUniqueStrings(result.dirtyMountIDs, record.MountID)
		}
		if action != nil {
			result.localRootActions = append(result.localRootActions, *action)
		}
	}

	return result
}

func reconcileChildMountRootRecord(
	inventory *config.MountInventory,
	record *config.MountRecord,
	parent *mountSpec,
	logger *slog.Logger,
) (bool, *childRootLifecycleAction) {
	root := filepath.Join(parent.syncRoot, filepath.FromSlash(record.RelativeLocalPath))
	decision := classifyChildMountRoot(parent, record)
	logChildRootLifecycle(record, root, decision.state, decision.reason, logger)

	changed := setMountLifecycleState(inventory, record, decision.state, decision.reason)
	if decision.localRootMaterialized != record.LocalRootMaterialized {
		record.LocalRootMaterialized = decision.localRootMaterialized
		changed = true
	}
	if !rootIdentityEqual(record.LocalRootIdentity, decision.identity) {
		record.LocalRootIdentity = cloneRootIdentity(decision.identity)
		changed = true
	}
	if !stringSlicesEqual(record.ReservedLocalPaths, decision.reservedLocalPaths) {
		record.ReservedLocalPaths = append([]string(nil), decision.reservedLocalPaths...)
		changed = true
	}
	if changed {
		inventory.Mounts[record.MountID] = *record
	}

	return changed, decision.action
}

func logChildRootLifecycle(
	record *config.MountRecord,
	root string,
	state config.MountState,
	reason string,
	logger *slog.Logger,
) {
	if logger == nil {
		return
	}
	if state == config.MountStateActive && record.State == config.MountStateConflict &&
		record.StateReason == config.MountStateReasonLocalRootCollision {
		logger.Info("child mount local root collision cleared",
			slog.String("mount_id", record.MountID),
			slog.String("path", root),
		)
		return
	}
	if state != config.MountStateActive {
		logger.Warn("child mount local root is not usable",
			slog.String("mount_id", record.MountID),
			slog.String("path", root),
			slog.String("state", string(state)),
			slog.String("reason", reason),
		)
	}
}

func childRootNeedsReconciliation(record *config.MountRecord) bool {
	if record == nil {
		return false
	}
	if len(record.ReservedLocalPaths) > 0 && !childRootReservedPathsAllowRetry(record) {
		return false
	}

	switch record.State {
	case "", config.MountStateActive:
		return true
	case config.MountStateConflict:
		return record.StateReason == config.MountStateReasonLocalRootCollision ||
			record.StateReason == config.MountStateReasonLocalAliasRenameConflict
	case config.MountStateUnavailable:
		return record.StateReason == config.MountStateReasonLocalRootUnavailable ||
			record.StateReason == config.MountStateReasonLocalAliasRenameUnavailable ||
			record.StateReason == config.MountStateReasonLocalAliasDeleteUnavailable
	case config.MountStatePendingRemoval:
		return false
	default:
		return false
	}
}

func childRootReservedPathsAllowRetry(record *config.MountRecord) bool {
	switch record.State {
	case config.MountStateConflict:
		return record.StateReason == config.MountStateReasonLocalAliasRenameConflict
	case config.MountStateUnavailable:
		return record.StateReason == config.MountStateReasonLocalAliasRenameUnavailable ||
			record.StateReason == config.MountStateReasonLocalAliasDeleteUnavailable
	case "", config.MountStateActive, config.MountStatePendingRemoval:
		return false
	default:
		return false
	}
}

func classifyChildMountRoot(parent *mountSpec, record *config.MountRecord) childRootLifecycleDecision {
	decision := childRootLifecycleDecision{
		state:                 config.MountStateActive,
		localRootMaterialized: record.LocalRootMaterialized,
		reservedLocalPaths:    append([]string(nil), record.ReservedLocalPaths...),
	}
	state, reason, identity, exists := classifyMaterializedChildMountRoot(parent.syncRoot, record)
	decision.state = state
	decision.reason = reason
	if state == config.MountStateActive {
		decision.localRootMaterialized = true
		decision.identity = identity
		decision.reservedLocalPaths = nil
		return decision
	}
	if !record.LocalRootMaterialized || exists || record.LocalRootIdentity == nil ||
		state != config.MountStateUnavailable ||
		reason != config.MountStateReasonLocalRootUnavailable {
		return decision
	}

	candidates, scanErr := findSameParentChildRootRenameCandidates(parent.syncRoot, record)
	if scanErr != nil {
		decision.state = config.MountStateUnavailable
		decision.reason = config.MountStateReasonLocalAliasRenameUnavailable
		return decision
	}
	switch len(candidates) {
	case 0:
		decision.action = &childRootLifecycleAction{
			kind:                  childRootLifecycleActionDelete,
			mountID:               mountID(record.MountID),
			parent:                parent,
			selectionIndex:        0,
			identity:              MountIdentity{MountID: record.MountID, ParentMountID: record.NamespaceID, ProjectionKind: MountProjectionChild},
			displayName:           record.LocalAlias,
			bindingItemID:         record.BindingItemID,
			fromRelativeLocalPath: record.RelativeLocalPath,
		}
		decision.state = config.MountStateActive
		decision.reason = ""
	case 1:
		decision.action = &childRootLifecycleAction{
			kind:                  childRootLifecycleActionRename,
			mountID:               mountID(record.MountID),
			parent:                parent,
			identity:              MountIdentity{MountID: record.MountID, ParentMountID: record.NamespaceID, ProjectionKind: MountProjectionChild},
			displayName:           record.LocalAlias,
			bindingItemID:         record.BindingItemID,
			fromRelativeLocalPath: record.RelativeLocalPath,
			toRelativeLocalPath:   candidates[0],
		}
		decision.state = config.MountStateActive
		decision.reason = ""
	default:
		decision.state = config.MountStateConflict
		decision.reason = config.MountStateReasonLocalAliasRenameConflict
		decision.reservedLocalPaths = appendUniqueStrings(decision.reservedLocalPaths, candidates...)
	}

	return decision
}

func materializeChildMountRoot(parentRoot string, record *config.MountRecord) (config.MountState, string) {
	state, reason, _, _ := classifyMaterializedChildMountRoot(parentRoot, record)
	return state, reason
}

func classifyMaterializedChildMountRoot(
	parentRoot string,
	record *config.MountRecord,
) (config.MountState, string, *config.RootIdentity, bool) {
	if record == nil {
		return config.MountStateConflict, config.MountStateReasonLocalRootCollision, nil, false
	}

	relativePath, ok := cleanChildMountRootRelativePath(record.RelativeLocalPath)
	if !ok {
		return config.MountStateConflict, config.MountStateReasonLocalRootCollision, nil, false
	}

	root, openErr := synctree.Open(parentRoot)
	if openErr != nil {
		return config.MountStateUnavailable, config.MountStateReasonLocalRootUnavailable, nil, false
	}
	if err := root.ValidateNoSymlinkAncestors(relativePath); err != nil {
		state, reason := childMountRootErrorState(err)
		return state, reason, nil, false
	}
	pathState, err := root.PathStateNoFollow(relativePath)
	if err != nil {
		state, reason := childMountRootErrorState(err)
		return state, reason, nil, false
	}
	if pathState.Exists {
		if !pathState.IsDir {
			return config.MountStateConflict, config.MountStateReasonLocalRootCollision, nil, true
		}

		identity, identityErr := childRootIdentity(root, relativePath)
		if identityErr != nil {
			return config.MountStateUnavailable, config.MountStateReasonLocalRootUnavailable, nil, true
		}
		return config.MountStateActive, "", identity, true
	}
	if record.LocalRootMaterialized {
		return config.MountStateUnavailable, config.MountStateReasonLocalRootUnavailable, nil, false
	}
	if err := root.MkdirAllNoFollow(relativePath, childMountRootDirPerms); err != nil {
		state, reason := childMountRootErrorState(err)
		return state, reason, nil, false
	}

	identity, identityErr := childRootIdentity(root, relativePath)
	if identityErr != nil {
		return config.MountStateUnavailable, config.MountStateReasonLocalRootUnavailable, nil, true
	}
	return config.MountStateActive, "", identity, true
}

func cleanChildMountRootRelativePath(relativeLocalPath string) (string, bool) {
	if relativeLocalPath == "" {
		return "", false
	}

	relativePath := filepath.Clean(filepath.FromSlash(relativeLocalPath))
	if filepath.IsAbs(relativePath) || relativePath == "." || relativePath == ".." {
		return "", false
	}
	if strings.HasPrefix(relativePath, ".."+string(os.PathSeparator)) {
		return "", false
	}

	return relativePath, true
}

func childMountRootErrorState(err error) (config.MountState, string) {
	if errors.Is(err, synctree.ErrUnsafePath) {
		return config.MountStateConflict, config.MountStateReasonLocalRootCollision
	}
	if errors.Is(err, os.ErrNotExist) {
		return config.MountStateUnavailable, config.MountStateReasonLocalRootUnavailable
	}
	if errors.Is(err, syscall.ENOTDIR) {
		return config.MountStateConflict, config.MountStateReasonLocalRootCollision
	}

	return config.MountStateUnavailable, config.MountStateReasonLocalRootUnavailable
}

func findSameParentChildRootRenameCandidates(
	parentRoot string,
	record *config.MountRecord,
) ([]string, error) {
	if record == nil || record.LocalRootIdentity == nil {
		return nil, nil
	}
	relativePath, ok := cleanChildMountRootRelativePath(record.RelativeLocalPath)
	if !ok {
		return nil, fmt.Errorf("resolving child mount root %q", record.RelativeLocalPath)
	}
	root, openErr := synctree.Open(parentRoot)
	if openErr != nil {
		return nil, fmt.Errorf("opening parent sync root: %w", openErr)
	}

	parentRel := filepath.Dir(relativePath)
	if parentRel == "." {
		parentRel = ""
	}
	if err := validateCandidateParent(root, parentRel); err != nil {
		return nil, err
	}
	entries, err := root.ReadDir(parentRel)
	if err != nil {
		return nil, fmt.Errorf("reading child root parent: %w", err)
	}

	candidates := make([]string, 0, 1)
	for _, entry := range entries {
		candidate, ok := sameParentChildRootRenameCandidate(root, parentRel, relativePath, entry, record.LocalRootIdentity)
		if ok {
			candidates = append(candidates, candidate)
		}
	}

	return candidates, nil
}

func validateCandidateParent(root *synctree.Root, parentRel string) error {
	if parentRel == "" {
		return nil
	}
	if err := root.ValidateNoSymlinkAncestors(parentRel); err != nil {
		return fmt.Errorf("validating child root parent: %w", err)
	}

	return nil
}

func sameParentChildRootRenameCandidate(
	root *synctree.Root,
	parentRel string,
	originalRel string,
	entry os.DirEntry,
	expected *config.RootIdentity,
) (string, bool) {
	name := entry.Name()
	candidateRel := name
	if parentRel != "" {
		candidateRel = filepath.Join(parentRel, name)
	}
	if candidateRel == originalRel {
		return "", false
	}
	info, infoErr := root.Lstat(candidateRel)
	if infoErr != nil || !info.IsDir() {
		return "", false
	}
	identity, identityErr := rootIdentityFromFileInfo(info)
	if identityErr != nil || !rootIdentityEqual(identity, expected) {
		return "", false
	}

	return filepath.ToSlash(candidateRel), true
}

func childRootIdentity(root *synctree.Root, relativePath string) (*config.RootIdentity, error) {
	info, err := root.Lstat(relativePath)
	if err != nil {
		return nil, fmt.Errorf("stating child root: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("local root %s is not a directory", relativePath)
	}

	return rootIdentityFromFileInfo(info)
}

func rootIdentityFromFileInfo(info fs.FileInfo) (*config.RootIdentity, error) {
	if info == nil {
		return nil, fmt.Errorf("file info is missing")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return nil, fmt.Errorf("file info has no stable device/inode identity")
	}
	identity := &config.RootIdentity{
		Device: statDeviceID(stat),
		Inode:  stat.Ino,
	}
	if identity.Device == 0 && identity.Inode == 0 {
		return nil, fmt.Errorf("file info has zero device/inode identity")
	}

	return identity, nil
}

func rootIdentityEqual(a *config.RootIdentity, b *config.RootIdentity) bool {
	switch {
	case a == nil || b == nil:
		return a == b
	default:
		return a.Device == b.Device && a.Inode == b.Inode
	}
}

func cloneRootIdentity(identity *config.RootIdentity) *config.RootIdentity {
	if identity == nil {
		return nil
	}
	return &config.RootIdentity{Device: identity.Device, Inode: identity.Inode}
}

func stringSlicesEqual(a []string, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func childRootActionAlias(action *childRootLifecycleAction) string {
	if action == nil || action.toRelativeLocalPath == "" {
		return ""
	}

	return path.Base(action.toRelativeLocalPath)
}

func validateCompiledChildMountRoots(compiled *compiledMountSet, logger *slog.Logger) {
	if compiled == nil || len(compiled.Mounts) == 0 {
		return
	}

	parentByID := standaloneParentMountsByID(compiled.Mounts)
	filtered := compiled.Mounts[:0]
	for _, mount := range compiled.Mounts {
		if mount == nil || mount.projectionKind != MountProjectionChild {
			filtered = append(filtered, mount)
			continue
		}

		parent := parentByID[mount.parentMountID]
		if parent == nil {
			filtered = append(filtered, mount)
			continue
		}

		state, reason, err := validateCompiledChildMountRoot(parent, mount)
		if err != nil {
			compiled.Skipped = append(compiled.Skipped, mountStartupResultForMount(mount, err))
			recordCompiledChildRootState(mount.mountID, state, reason, logger)
			continue
		}

		filtered = append(filtered, mount)
	}
	compiled.Mounts = filtered
}

func standaloneParentMountsByID(mounts []*mountSpec) map[mountID]*mountSpec {
	parents := make(map[mountID]*mountSpec)
	for i := range mounts {
		if mounts[i] == nil || mounts[i].projectionKind != MountProjectionStandalone {
			continue
		}
		parents[mounts[i].mountID] = mounts[i]
	}

	return parents
}

func validateCompiledChildMountRoot(
	parent *mountSpec,
	child *mountSpec,
) (config.MountState, string, error) {
	relativeLocalPath, err := childMountRelativePath(parent, child)
	if err != nil {
		return config.MountStateUnavailable,
			config.MountStateReasonLocalRootUnavailable,
			fmt.Errorf("child mount %s local root: %w", child.mountID, err)
	}

	record := &config.MountRecord{
		MountID:               child.mountID.String(),
		RelativeLocalPath:     relativeLocalPath,
		LocalRootMaterialized: true,
	}
	state, reason := materializeChildMountRoot(parent.syncRoot, record)
	if state == config.MountStateActive {
		return state, reason, nil
	}

	return state, reason, childRootStateError(child.mountID, state, reason)
}

func childMountRelativePath(parent *mountSpec, child *mountSpec) (string, error) {
	parentRoot, err := filepath.Abs(filepath.Clean(parent.syncRoot))
	if err != nil {
		return "", fmt.Errorf("resolving parent sync root %q: %w", parent.syncRoot, err)
	}
	childRoot, err := filepath.Abs(filepath.Clean(child.syncRoot))
	if err != nil {
		return "", fmt.Errorf("resolving child sync root %q: %w", child.syncRoot, err)
	}

	relativePath, err := filepath.Rel(parentRoot, childRoot)
	if err != nil {
		return "", fmt.Errorf("relativizing child sync root %q: %w", child.syncRoot, err)
	}
	relativePath = filepath.ToSlash(relativePath)
	if _, ok := cleanChildMountRootRelativePath(relativePath); !ok {
		return "", fmt.Errorf("path %q escapes parent sync root", relativePath)
	}

	return relativePath, nil
}

func childRootStateError(mountID mountID, state config.MountState, reason string) error {
	switch state {
	case config.MountStateActive:
		return nil
	case config.MountStateConflict:
		if reason != "" {
			return fmt.Errorf("child mount %s is conflicted: %s", mountID, reason)
		}
		return fmt.Errorf("child mount %s is conflicted", mountID)
	case config.MountStatePendingRemoval:
		return fmt.Errorf("child mount %s is pending removal", mountID)
	case config.MountStateUnavailable:
		if reason != "" {
			return fmt.Errorf("child mount %s is unavailable: %s", mountID, reason)
		}
		return fmt.Errorf("child mount %s is unavailable", mountID)
	default:
		return fmt.Errorf("child mount %s local root has unsupported state %q", mountID, state)
	}
}

func recordCompiledChildRootState(
	childMountID mountID,
	state config.MountState,
	reason string,
	logger *slog.Logger,
) {
	if err := config.UpdateMountInventory(func(inventory *config.MountInventory) error {
		record, found := inventory.Mounts[childMountID.String()]
		if !found {
			return nil
		}
		setMountLifecycleState(inventory, &record, state, reason)
		return nil
	}); err != nil && logger != nil {
		logger.Warn("recording child mount local root failure",
			slog.String("mount_id", childMountID.String()),
			slog.String("error", err.Error()),
		)
	}
}
