package sync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

const (
	shortcutRootDirPerm             = os.FileMode(0o700)
	shortcutRootCaseRenameAttempts  = 10
	shortcutRootCaseRenameTempInfix = ".shortcut-alias-case-rename"
)

func (e *Engine) reconcileShortcutRootLocalState(ctx context.Context) (bool, error) {
	if e == nil || e.baseline == nil {
		return false, nil
	}
	records, err := e.baseline.ListShortcutRoots(ctx)
	if err != nil {
		return false, fmt.Errorf("sync: read shortcut roots for local lifecycle: %w", err)
	}
	localRows, err := e.baseline.ListLocalState(ctx)
	if err != nil {
		return false, fmt.Errorf("sync: read local_state for shortcut root local lifecycle: %w", err)
	}
	changed := false
	nextRecords := make([]ShortcutRootRecord, 0, len(records))
	for i := range records {
		next, keep, recordChanged, recordErr := e.reconcileShortcutRootRecord(ctx, records[i], localRows)
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
	if err := e.refreshProtectedRootsFromStore(ctx); err != nil {
		return false, fmt.Errorf("sync: refresh shortcut protected roots: %w", err)
	}
	return true, nil
}

//nolint:gocritic // ShortcutRootRecord is treated as a value in the planner-style local transition.
func (e *Engine) reconcileShortcutRootRecord(
	ctx context.Context,
	record ShortcutRootRecord,
	localRows []LocalStateRow,
) (ShortcutRootRecord, bool, bool, error) {
	record = normalizeShortcutRootRecord(record)
	switch record.State {
	case "",
		ShortcutRootStateActive,
		ShortcutRootStateTargetUnavailable,
		ShortcutRootStateBlockedPath,
		ShortcutRootStateRenameAmbiguous,
		ShortcutRootStateAliasMutationBlocked,
		ShortcutRootStateDuplicateTarget:
	case ShortcutRootStateRemovedFinalDrain,
		ShortcutRootStateSamePathReplacementWaiting:
		return e.reconcileRetiringShortcutRootLocalState(record)
	case ShortcutRootStateRemovedReleasePending,
		ShortcutRootStateRemovedCleanupBlocked:
		next, keep, changed := e.retryShortcutRootReleaseCleanup(&record)
		return next, keep, changed, nil
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
	return e.reconcileMissingMaterializedShortcutRoot(ctx, record, relativePath, localRows)
}

//nolint:gocritic // ShortcutRootRecord is treated as a value in local transition helpers.
func (e *Engine) reconcileRetiringShortcutRootLocalState(
	record ShortcutRootRecord,
) (ShortcutRootRecord, bool, bool, error) {
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
			return shortcutRootCleanupBlocked(record, fmt.Errorf("shortcut alias path is not a directory")), true, true, nil
		}
		return record, true, false, nil
	}
	if record.State == ShortcutRootStateSamePathReplacementWaiting && record.Waiting != nil {
		return shortcutRootRecordFromReplacement(record.NamespaceID, *record.Waiting), true, true, nil
	}
	return ShortcutRootRecord{}, false, true, nil
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
	localRows []LocalStateRow,
) (ShortcutRootRecord, bool, bool, error) {
	candidates, candidateErr := e.shortcutRootIdentityCandidates(relativePath, *record.LocalRootIdentity, localRows)
	if candidateErr != nil {
		return unavailableShortcutRoot(record, candidateErr.Error()), true, true, nil
	}
	if previousPath, ok := previousProtectedProjectionCandidate(&record, candidates); ok {
		return e.moveRemoteRenamedShortcutProjection(record, previousPath, relativePath)
	}
	switch len(candidates) {
	case 0:
		if err := e.applyShortcutAliasMutation(ctx, shortcutAliasMutation{
			Kind:          shortcutAliasMutationDelete,
			BindingItemID: record.BindingItemID,
		}); err != nil {
			return aliasMutationBlockedShortcutRoot(record, err), true, true, nil
		}
		return ShortcutRootRecord{}, false, true, nil
	case 1:
		alias := path.Base(candidates[0])
		if err := e.applyShortcutAliasMutation(ctx, shortcutAliasMutation{
			Kind:              shortcutAliasMutationRename,
			BindingItemID:     record.BindingItemID,
			RelativeLocalPath: candidates[0],
			LocalAlias:        alias,
		}); err != nil {
			next := aliasMutationBlockedShortcutRoot(record, err)
			next.ProtectedPaths = appendUniqueProtectedRootPaths(next.ProtectedPaths, candidates[0])
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
		next.ProtectedPaths = appendUniqueProtectedRootPaths(next.ProtectedPaths, candidates...)
		return next, true, true, nil
	}
}

func (e *Engine) shortcutRootIdentityCandidates(
	relativePath string,
	identity synctree.FileIdentity,
	localRows []LocalStateRow,
) ([]string, error) {
	candidates := shortcutRootIdentityCandidatesFromLocalState(relativePath, identity, localRows)
	liveCandidates, err := e.shortcutRootIdentityCandidatesFromFilesystem(relativePath, identity)
	if err != nil {
		return nil, err
	}
	candidates = append(candidates, liveCandidates...)
	return uniqueSortedShortcutRootCandidates(candidates), nil
}

func shortcutRootIdentityCandidatesFromLocalState(
	relativePath string,
	identity synctree.FileIdentity,
	localRows []LocalStateRow,
) []string {
	normalizedCurrent := filepath.ToSlash(relativePath)
	candidates := make([]string, 0)
	for i := range localRows {
		row := localRows[i]
		if row.ItemType != ItemTypeFolder || !row.LocalHasIdentity {
			continue
		}
		if row.Path == normalizedCurrent {
			continue
		}
		if row.LocalDevice != identity.Device || row.LocalInode != identity.Inode {
			continue
		}
		candidates = append(candidates, row.Path)
	}
	slices.Sort(candidates)
	return candidates
}

func (e *Engine) shortcutRootIdentityCandidatesFromFilesystem(
	relativePath string,
	identity synctree.FileIdentity,
) ([]string, error) {
	if e == nil || e.syncTree == nil {
		return nil, nil
	}
	normalizedCurrent := normalizedProtectedRootPath(relativePath)
	parent := path.Dir(normalizedCurrent)
	if parent == "." {
		parent = ""
	}
	entries, err := e.syncTree.ReadDir(parent)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read shortcut alias parent directory: %w", err)
	}
	candidates := make([]string, 0)
	for _, entry := range entries {
		candidate, found, err := e.shortcutRootIdentityCandidateFromFilesystemEntry(
			parent,
			normalizedCurrent,
			entry.Name(),
			identity,
		)
		if err != nil {
			return nil, err
		}
		if found {
			candidates = append(candidates, candidate)
		}
	}
	return uniqueSortedShortcutRootCandidates(candidates), nil
}

func (e *Engine) shortcutRootIdentityCandidateFromFilesystemEntry(
	parent string,
	normalizedCurrent string,
	entryName string,
	identity synctree.FileIdentity,
) (string, bool, error) {
	candidate := entryName
	if parent != "" {
		candidate = path.Join(parent, candidate)
	}
	if candidate == normalizedCurrent {
		return "", false, nil
	}
	state, err := e.syncTree.PathStateNoFollow(candidate)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("stat shortcut alias rename candidate: %w", err)
	}
	if !state.Exists || !state.IsDir {
		return "", false, nil
	}
	candidateIdentity, err := e.syncTree.IdentityNoFollow(candidate)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("read shortcut alias rename candidate identity: %w", err)
	}
	if !synctree.SameIdentity(identity, candidateIdentity) {
		return "", false, nil
	}
	return candidate, true, nil
}

func uniqueSortedShortcutRootCandidates(candidates []string) []string {
	if len(candidates) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(candidates))
	unique := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		normalized := normalizedProtectedRootPath(candidate)
		if normalized == "" {
			continue
		}
		if _, ok := seen[normalized]; ok {
			continue
		}
		seen[normalized] = struct{}{}
		unique = append(unique, normalized)
	}
	slices.Sort(unique)
	return unique
}

func previousProtectedProjectionCandidate(record *ShortcutRootRecord, candidates []string) (string, bool) {
	if record == nil || len(candidates) == 0 {
		return "", false
	}
	protected := make(map[string]struct{}, len(record.ProtectedPaths))
	for _, protectedPath := range record.ProtectedPaths {
		normalized := normalizedProtectedRootPath(protectedPath)
		if normalized == "" || normalized == record.RelativeLocalPath {
			continue
		}
		protected[normalized] = struct{}{}
	}
	for _, candidate := range candidates {
		normalized := normalizedProtectedRootPath(candidate)
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
	if err := e.validateShortcutRootProjectionMove(fromRelativePath, toRelativePath); err != nil {
		return err
	}
	toState, err := e.syncTree.PathStateNoFollow(toRelativePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stating current shortcut alias projection: %w", err)
	}
	if toState.Exists {
		moved, err := e.moveShortcutRootProjectionOntoExistingTarget(fromRelativePath, toRelativePath)
		if err != nil {
			return err
		}
		if moved {
			return nil
		}
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

func (e *Engine) moveShortcutRootProjectionOntoExistingTarget(
	fromRelativePath string,
	toRelativePath string,
) (bool, error) {
	same, sameErr := e.syncTree.SameFile(fromRelativePath, toRelativePath)
	if sameErr != nil {
		return false, fmt.Errorf("comparing shortcut alias projection identity: %w", sameErr)
	}
	if !same {
		return false, nil
	}
	if err := e.syncTree.RenameWithTemporarySibling(
		fromRelativePath,
		toRelativePath,
		shortcutRootCaseRenameTempInfix,
		shortcutRootCaseRenameAttempts,
	); err != nil {
		return false, fmt.Errorf("moving case-only shortcut alias projection: %w", err)
	}
	return true, nil
}

func (e *Engine) validateShortcutRootProjectionMove(fromRelativePath string, toRelativePath string) error {
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
	return nil
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
	return e.finalizeShortcutRootReleaseByBinding(ctx, ack.BindingItemID)
}

func shortcutRootStateAwaitsFinalDrainAck(state ShortcutRootState) bool {
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
		ShortcutRootStateAliasMutationBlocked,
		ShortcutRootStateRemovedReleasePending,
		ShortcutRootStateDuplicateTarget:
		return false
	default:
		return false
	}
}

func shortcutRootStateAwaitsReleaseCleanup(state ShortcutRootState) bool {
	switch state {
	case ShortcutRootStateRemovedReleasePending,
		ShortcutRootStateRemovedCleanupBlocked:
		return true
	case "",
		ShortcutRootStateActive,
		ShortcutRootStateTargetUnavailable,
		ShortcutRootStateBlockedPath,
		ShortcutRootStateRenameAmbiguous,
		ShortcutRootStateAliasMutationBlocked,
		ShortcutRootStateRemovedFinalDrain,
		ShortcutRootStateSamePathReplacementWaiting,
		ShortcutRootStateDuplicateTarget:
		return false
	default:
		return false
	}
}

func (e *Engine) retryShortcutRootReleaseCleanup(record *ShortcutRootRecord) (ShortcutRootRecord, bool, bool) {
	if record == nil {
		return ShortcutRootRecord{}, false, false
	}
	next, keep, changed, cleanupErr := e.finalizeShortcutRootReleaseRecord(*record)
	if cleanupErr != nil {
		changed = true
		return next, keep, changed
	}
	return next, keep, changed
}

func (e *Engine) finalizeShortcutRootReleaseByBinding(ctx context.Context, bindingItemID string) error {
	records, err := e.baseline.ListShortcutRoots(ctx)
	if err != nil {
		return fmt.Errorf("sync: read shortcut roots before child drain release: %w", err)
	}
	changed := false
	nextRecords := make([]ShortcutRootRecord, 0, len(records))
	var cleanupErr error
	for i := range records {
		record := normalizeShortcutRootRecord(records[i])
		if record.BindingItemID != bindingItemID || !shortcutRootStateAwaitsReleaseCleanup(record.State) {
			nextRecords = append(nextRecords, record)
			continue
		}
		next, keep, recordChanged, recordErr := e.finalizeShortcutRootReleaseRecord(record)
		changed = changed || recordChanged
		if keep {
			nextRecords = append(nextRecords, next)
		}
		if recordErr != nil {
			cleanupErr = recordErr
		}
	}
	if changed {
		if err := e.baseline.ReplaceShortcutRoots(ctx, nextRecords); err != nil {
			return fmt.Errorf("sync: persist shortcut root release: %w", err)
		}
	}
	if cleanupErr != nil {
		return fmt.Errorf("sync: release shortcut root projection: %w", cleanupErr)
	}
	return nil
}

//nolint:gocritic // ShortcutRootRecord is a value-shaped lifecycle plan output.
func (e *Engine) finalizeShortcutRootReleaseRecord(
	record ShortcutRootRecord,
) (ShortcutRootRecord, bool, bool, error) {
	record = normalizeShortcutRootRecord(record)
	if !shortcutRootStateAwaitsReleaseCleanup(record.State) {
		return record, true, false, nil
	}
	if err := e.removeShortcutRootProjection(record.RelativeLocalPath); err != nil {
		next := shortcutRootCleanupBlocked(record, err)
		return next, true, !shortcutRootRecordsEqual(record, next), err
	}
	if record.Waiting != nil {
		next := shortcutRootRecordFromReplacement(record.NamespaceID, *record.Waiting)
		return next, true, true, nil
	}
	return ShortcutRootRecord{}, false, true, nil
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

func appendUniqueProtectedRootPaths(paths []string, additions ...string) []string {
	merged := append([]string(nil), paths...)
	for _, addition := range additions {
		normalized := normalizedProtectedRootPath(addition)
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
