package sync

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

const shortcutRootDirPerm = os.FileMode(0o700)

func (e *Engine) reconcileShortcutRootLocalState(ctx context.Context) (bool, error) {
	if e == nil || e.hasRemoteMountRoot() || e.baseline == nil {
		return false, nil
	}
	records, err := e.baseline.ListShortcutRoots(ctx)
	if err != nil {
		return false, fmt.Errorf("sync: read shortcut roots for local lifecycle: %w", err)
	}
	changed := false
	nextRecords := make([]ShortcutRootRecord, 0, len(records))
	for i := range records {
		next, keep, recordChanged, recordErr := e.reconcileShortcutRootRecord(ctx, records[i])
		if recordErr != nil {
			return false, recordErr
		}
		if keep {
			nextRecords = append(nextRecords, next)
		}
		changed = changed || recordChanged
	}
	if !changed {
		return false, nil
	}
	if err := e.baseline.ReplaceShortcutRoots(ctx, nextRecords); err != nil {
		return false, fmt.Errorf("sync: persist shortcut root local lifecycle: %w", err)
	}
	e.localFilter.ManagedRoots = mergeManagedRootReservations(
		e.localFilter.ManagedRoots,
		managedRootReservationsForShortcutRoots(nextRecords, e.shortcutTopologyNamespaceID),
	)
	return true, nil
}

//nolint:gocritic // ShortcutRootRecord is treated as a value in the planner-style local transition.
func (e *Engine) reconcileShortcutRootRecord(
	ctx context.Context,
	record ShortcutRootRecord,
) (ShortcutRootRecord, bool, bool, error) {
	record = normalizeShortcutRootRecord(record)
	switch record.State {
	case "",
		ShortcutRootStateActive,
		ShortcutRootStateTargetUnavailable,
		ShortcutRootStateBlockedPath,
		ShortcutRootStateRenameAmbiguous,
		ShortcutRootStateAliasMutationBlocked:
	case ShortcutRootStateRemovedFinalDrain,
		ShortcutRootStateRemovedCleanupBlocked,
		ShortcutRootStateSamePathReplacementWaiting:
		return record, true, false, nil
	}
	relativePath, ok := cleanShortcutRootRelativePath(record.RelativeLocalPath)
	if !ok {
		return blockedShortcutRoot(record, "shortcut alias path escapes parent sync root"), true, true, nil
	}
	if err := e.syncTree.ValidateNoSymlinkAncestors(relativePath); err != nil {
		return shortcutRootWithPathError(record, err), true, true, nil
	}
	state, err := e.syncTree.PathStateNoFollow(relativePath)
	if err != nil {
		return shortcutRootWithPathError(record, err), true, true, nil
	}
	if state.Exists {
		if !state.IsDir {
			return blockedShortcutRoot(record, "shortcut alias path is not a directory"), true, true, nil
		}
		identity, identityErr := e.syncTree.IdentityNoFollow(relativePath)
		if identityErr != nil {
			return unavailableShortcutRoot(record, identityErr.Error()), true, true, nil
		}
		next := record
		next.LocalRootIdentity = &identity
		if next.State == ShortcutRootStateBlockedPath ||
			next.State == ShortcutRootStateRenameAmbiguous ||
			next.State == ShortcutRootStateAliasMutationBlocked {
			next = plannedShortcutRootTransition(next,
				shortcutRootEventLocalRootReady,
				ShortcutRootStateActive,
				"",
			)
		}
		return next, true, !shortcutRootRecordsEqual(record, next), nil
	}
	if record.LocalRootIdentity == nil {
		return e.materializeShortcutRoot(record, relativePath)
	}
	return e.reconcileMissingMaterializedShortcutRoot(ctx, record, relativePath)
}

//nolint:gocritic // ShortcutRootRecord is treated as a value in the planner-style local transition.
func (e *Engine) materializeShortcutRoot(
	record ShortcutRootRecord,
	relativePath string,
) (ShortcutRootRecord, bool, bool, error) {
	if err := e.syncTree.MkdirAllNoFollow(relativePath, shortcutRootDirPerm); err != nil {
		return shortcutRootWithPathError(record, err), true, true, nil
	}
	identity, err := e.syncTree.IdentityNoFollow(relativePath)
	if err != nil {
		return unavailableShortcutRoot(record, err.Error()), true, true, nil
	}
	next := record
	next = plannedShortcutRootTransition(next,
		shortcutRootEventLocalRootReady,
		ShortcutRootStateActive,
		"",
	)
	next.LocalRootIdentity = &identity
	next.ProtectedPaths = protectedPathsForShortcutRoot(next.RelativeLocalPath, next.ProtectedPaths)
	return next, true, !shortcutRootRecordsEqual(record, next), nil
}

//nolint:gocritic // ShortcutRootRecord is treated as a value in the planner-style local transition.
func (e *Engine) reconcileMissingMaterializedShortcutRoot(
	ctx context.Context,
	record ShortcutRootRecord,
	relativePath string,
) (ShortcutRootRecord, bool, bool, error) {
	candidates, err := e.findSameParentShortcutRootRenameCandidates(relativePath, *record.LocalRootIdentity)
	if err != nil {
		return unavailableShortcutRoot(record, err.Error()), true, true, nil
	}
	if previousPath, ok := previousProtectedProjectionCandidate(&record, candidates); ok {
		return e.moveRemoteRenamedShortcutProjection(record, previousPath, relativePath)
	}
	switch len(candidates) {
	case 0:
		if err := e.ApplyShortcutAliasMutation(ctx, ShortcutAliasMutation{
			Kind:          ShortcutAliasMutationDelete,
			BindingItemID: record.BindingItemID,
		}); err != nil {
			return aliasMutationBlockedShortcutRoot(record, err), true, true, nil
		}
		return ShortcutRootRecord{}, false, true, nil
	case 1:
		alias := path.Base(candidates[0])
		if err := e.ApplyShortcutAliasMutation(ctx, ShortcutAliasMutation{
			Kind:              ShortcutAliasMutationRename,
			BindingItemID:     record.BindingItemID,
			RelativeLocalPath: candidates[0],
			LocalAlias:        alias,
		}); err != nil {
			next := aliasMutationBlockedShortcutRoot(record, err)
			next.ProtectedPaths = appendUniqueManagedRootPaths(next.ProtectedPaths, candidates[0])
			return next, true, true, nil
		}
		identity, identityErr := e.syncTree.IdentityNoFollow(filepath.FromSlash(candidates[0]))
		if identityErr != nil {
			return unavailableShortcutRoot(record, identityErr.Error()), true, true, nil
		}
		next := record
		next.RelativeLocalPath = candidates[0]
		next.LocalAlias = alias
		next = plannedShortcutRootTransition(next,
			shortcutRootEventLocalRootReady,
			ShortcutRootStateActive,
			"",
		)
		next.LocalRootIdentity = &identity
		next.ProtectedPaths = protectedPathsForShortcutRoot(next.RelativeLocalPath, append(record.ProtectedPaths, record.RelativeLocalPath))
		return next, true, true, nil
	default:
		next := plannedShortcutRootTransition(record,
			shortcutRootEventAliasRenameAmbiguous,
			ShortcutRootStateRenameAmbiguous,
			"multiple same-parent shortcut alias rename candidates",
		)
		next.ProtectedPaths = appendUniqueManagedRootPaths(next.ProtectedPaths, candidates...)
		return next, true, true, nil
	}
}

func previousProtectedProjectionCandidate(record *ShortcutRootRecord, candidates []string) (string, bool) {
	if record == nil || len(candidates) == 0 {
		return "", false
	}
	protected := make(map[string]struct{}, len(record.ProtectedPaths))
	for _, protectedPath := range record.ProtectedPaths {
		normalized := normalizedManagedRootPath(protectedPath)
		if normalized == "" || normalized == record.RelativeLocalPath {
			continue
		}
		protected[normalized] = struct{}{}
	}
	for _, candidate := range candidates {
		normalized := normalizedManagedRootPath(candidate)
		if _, ok := protected[normalized]; ok {
			return normalized, true
		}
	}
	return "", false
}

//nolint:gocritic // ShortcutRootRecord is treated as a value in local transition helpers.
func (e *Engine) moveRemoteRenamedShortcutProjection(
	record ShortcutRootRecord,
	fromRelativePath string,
	toRelativePath string,
) (ShortcutRootRecord, bool, bool, error) {
	if err := e.moveShortcutRootProjection(fromRelativePath, toRelativePath); err != nil {
		return blockedShortcutRoot(record, err.Error()), true, true, nil
	}
	identity, err := e.syncTree.IdentityNoFollow(toRelativePath)
	if err != nil {
		return unavailableShortcutRoot(record, err.Error()), true, true, nil
	}
	next := record
	next = plannedShortcutRootTransition(next,
		shortcutRootEventLocalRootReady,
		ShortcutRootStateActive,
		"",
	)
	next.LocalRootIdentity = &identity
	next.ProtectedPaths = protectedPathsForShortcutRoot(next.RelativeLocalPath, nil)
	return next, true, true, nil
}

func (e *Engine) moveShortcutRootProjection(fromRelativePath string, toRelativePath string) error {
	if fromRelativePath == "" || toRelativePath == "" || fromRelativePath == toRelativePath {
		return nil
	}
	if err := e.syncTree.ValidateNoSymlinkAncestors(fromRelativePath); err != nil {
		return fmt.Errorf("validating previous shortcut alias projection: %w", err)
	}
	if err := e.syncTree.ValidateNoSymlinkAncestors(filepath.Dir(toRelativePath)); err != nil {
		return fmt.Errorf("validating current shortcut alias parent: %w", err)
	}
	fromState, err := e.syncTree.PathStateNoFollow(fromRelativePath)
	if err != nil {
		return fmt.Errorf("stating previous shortcut alias projection: %w", err)
	}
	if !fromState.Exists || !fromState.IsDir {
		return fmt.Errorf("%w: previous shortcut alias projection is not a directory", synctree.ErrUnsafePath)
	}
	toState, err := e.syncTree.PathStateNoFollow(toRelativePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stating current shortcut alias projection: %w", err)
	}
	if toState.Exists {
		return fmt.Errorf("%w: current shortcut alias projection already exists", synctree.ErrUnsafePath)
	}
	if err := e.syncTree.MkdirAllNoFollow(filepath.Dir(toRelativePath), shortcutRootDirPerm); err != nil {
		return fmt.Errorf("creating current shortcut alias parent: %w", err)
	}
	if err := e.syncTree.Rename(fromRelativePath, toRelativePath); err != nil {
		return fmt.Errorf("moving shortcut alias projection: %w", err)
	}
	return nil
}

func (e *Engine) findSameParentShortcutRootRenameCandidates(
	relativePath string,
	identity synctree.FileIdentity,
) ([]string, error) {
	parentRel := filepath.Dir(relativePath)
	if err := e.syncTree.ValidateNoSymlinkAncestors(parentRel); err != nil {
		return nil, fmt.Errorf("validating shortcut alias parent: %w", err)
	}
	entries, err := e.syncTree.ReadDir(parentRel)
	if err != nil {
		return nil, fmt.Errorf("reading shortcut alias parent: %w", err)
	}
	candidates := make([]string, 0)
	for _, entry := range entries {
		candidate, ok := sameParentShortcutRootRenameCandidate(e.syncTree, parentRel, relativePath, entry, identity)
		if ok {
			candidates = append(candidates, candidate)
		}
	}
	return candidates, nil
}

func sameParentShortcutRootRenameCandidate(
	root *synctree.Root,
	parentRel string,
	originalRel string,
	entry fs.DirEntry,
	identity synctree.FileIdentity,
) (string, bool) {
	if root == nil || entry == nil || !entry.IsDir() {
		return "", false
	}
	name := entry.Name()
	candidateRel := name
	if parentRel != "." {
		candidateRel = filepath.Join(parentRel, name)
	}
	if candidateRel == originalRel {
		return "", false
	}
	candidateIdentity, err := root.IdentityNoFollow(candidateRel)
	if err != nil || !synctree.SameIdentity(candidateIdentity, identity) {
		return "", false
	}
	return filepath.ToSlash(candidateRel), true
}

func cleanShortcutRootRelativePath(relativeLocalPath string) (string, bool) {
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

//nolint:gocritic // ShortcutRootRecord is treated as a value in local transition helpers.
func shortcutRootWithPathError(record ShortcutRootRecord, err error) ShortcutRootRecord {
	if errors.Is(err, synctree.ErrUnsafePath) ||
		errors.Is(err, syscall.ENOTDIR) {
		return blockedShortcutRoot(record, err.Error())
	}
	if errors.Is(err, os.ErrNotExist) {
		return unavailableShortcutRoot(record, err.Error())
	}
	return unavailableShortcutRoot(record, err.Error())
}

//nolint:gocritic // ShortcutRootRecord is treated as a value in local transition helpers.
func blockedShortcutRoot(record ShortcutRootRecord, detail string) ShortcutRootRecord {
	return plannedShortcutRootTransition(record,
		shortcutRootEventLocalPathBlocked,
		ShortcutRootStateBlockedPath,
		detail,
	)
}

//nolint:gocritic // ShortcutRootRecord is treated as a value in local transition helpers.
func unavailableShortcutRoot(record ShortcutRootRecord, detail string) ShortcutRootRecord {
	return plannedShortcutRootTransition(record,
		shortcutRootEventLocalPathBlocked,
		ShortcutRootStateBlockedPath,
		detail,
	)
}

//nolint:gocritic // ShortcutRootRecord is treated as a value in local transition helpers.
func aliasMutationBlockedShortcutRoot(record ShortcutRootRecord, err error) ShortcutRootRecord {
	detail := ""
	if err != nil {
		detail = err.Error()
	}
	return plannedShortcutRootTransition(record,
		shortcutRootEventAliasMutationFailed,
		ShortcutRootStateAliasMutationBlocked,
		detail,
	)
}

func (e *Engine) releaseShortcutRootProjectionAfterDrain(ctx context.Context, ack ShortcutChildDrainAck) error {
	if e == nil || e.baseline == nil || ack.BindingItemID == "" {
		return nil
	}
	records, err := e.baseline.ListShortcutRoots(ctx)
	if err != nil {
		return fmt.Errorf("sync: read shortcut roots before child drain release: %w", err)
	}
	for i := range records {
		record := normalizeShortcutRootRecord(records[i])
		if record.BindingItemID != ack.BindingItemID {
			continue
		}
		if !shortcutRootStateAwaitsFinalDrainRelease(record.State) {
			return nil
		}
		if err := e.removeShortcutRootProjection(record.RelativeLocalPath); err != nil {
			records[i] = shortcutRootCleanupBlocked(record, err)
			if replaceErr := e.baseline.ReplaceShortcutRoots(ctx, records); replaceErr != nil {
				return fmt.Errorf("sync: persist blocked shortcut root cleanup: %w", replaceErr)
			}
			return fmt.Errorf("sync: release shortcut root projection: %w", err)
		}
		return nil
	}
	return nil
}

func shortcutRootStateAwaitsFinalDrainRelease(state ShortcutRootState) bool {
	switch state {
	case ShortcutRootStateRemovedFinalDrain,
		ShortcutRootStateRemovedCleanupBlocked,
		ShortcutRootStateSamePathReplacementWaiting:
		return true
	case "",
		ShortcutRootStateActive,
		ShortcutRootStateTargetUnavailable,
		ShortcutRootStateBlockedPath,
		ShortcutRootStateRenameAmbiguous,
		ShortcutRootStateAliasMutationBlocked:
		return false
	default:
		return false
	}
}

func (e *Engine) removeShortcutRootProjection(relativeLocalPath string) error {
	relativePath, ok := cleanShortcutRootRelativePath(relativeLocalPath)
	if !ok {
		return fmt.Errorf("%w: shortcut alias path escapes parent sync root", synctree.ErrUnsafePath)
	}
	if err := e.syncTree.ValidateNoSymlinkAncestors(relativePath); err != nil {
		return fmt.Errorf("validating shortcut alias projection: %w", err)
	}
	state, err := e.syncTree.PathStateNoFollow(relativePath)
	if err != nil {
		return fmt.Errorf("stating shortcut alias projection: %w", err)
	}
	if !state.Exists {
		return nil
	}
	if !state.IsDir {
		return fmt.Errorf("%w: shortcut alias path is not a directory", synctree.ErrUnsafePath)
	}
	if err := e.syncTree.RemoveTreeNoFollow(relativePath); err != nil {
		return fmt.Errorf("removing shortcut alias projection: %w", err)
	}
	return nil
}

//nolint:gocritic // ShortcutRootRecord is treated as a value in local transition helpers.
func shortcutRootCleanupBlocked(record ShortcutRootRecord, err error) ShortcutRootRecord {
	detail := ""
	if err != nil {
		detail = err.Error()
	}
	return plannedShortcutRootTransition(record,
		shortcutRootEventProjectionCleanupFailed,
		ShortcutRootStateRemovedCleanupBlocked,
		detail,
	)
}

func appendUniqueManagedRootPaths(paths []string, additions ...string) []string {
	merged := append([]string(nil), paths...)
	for _, addition := range additions {
		normalized := normalizedManagedRootPath(addition)
		if normalized == "" {
			continue
		}
		if slices.Contains(merged, normalized) {
			continue
		}
		merged = append(merged, normalized)
	}
	return merged
}
