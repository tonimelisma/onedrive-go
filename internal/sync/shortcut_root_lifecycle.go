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

	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

const (
	shortcutRootDirPerm             = os.FileMode(0o700)
	shortcutRootCaseRenameAttempts  = 10
	shortcutRootCaseRenameTempInfix = ".shortcut-alias-case-rename"
)

// This file is the shortcut-root local lifecycle shell. It gathers filesystem
// and store facts, executes Graph/filesystem effects, and persists planner
// outputs. Durable shortcut policy decisions stay in shortcut_root_planner_*.
func (e *Engine) reconcileShortcutRootLocalState(ctx context.Context) (bool, error) {
	if e == nil || e.baseline == nil {
		return false, nil
	}
	records, err := e.baseline.listShortcutRoots(ctx)
	if err != nil {
		return false, fmt.Errorf("sync: read shortcut roots for local lifecycle: %w", err)
	}
	localRows, err := e.baseline.ListLocalState(ctx)
	if err != nil {
		return false, fmt.Errorf("sync: read local_state for shortcut root local lifecycle: %w", err)
	}
	changed := false
	nextRecords := make([]ShortcutRootRecord, 0, len(records))
	var releaseCleanupErr error
	for i := range records {
		step, recordErr := e.reconcileShortcutRootLocalStateRecord(ctx, &records[i], localRows)
		if recordErr != nil {
			return false, recordErr
		}
		nextRecords = append(nextRecords, step.records...)
		changed = changed || step.changed
		releaseCleanupErr = errors.Join(releaseCleanupErr, step.releaseCleanupErr)
	}
	if !changed {
		if releaseCleanupErr != nil {
			return false, releaseCleanupErr
		}
		return false, nil
	}
	if err := e.baseline.replaceShortcutRoots(ctx, nextRecords); err != nil {
		return false, fmt.Errorf("sync: persist shortcut root local lifecycle: %w", err)
	}
	if err := e.refreshProtectedRootsFromStore(ctx); err != nil {
		return false, fmt.Errorf("sync: refresh shortcut protected roots: %w", err)
	}
	if releaseCleanupErr != nil {
		return false, releaseCleanupErr
	}
	return true, nil
}

type shortcutRootLocalReconcileStep struct {
	records           []ShortcutRootRecord
	changed           bool
	releaseCleanupErr error
}

func (e *Engine) reconcileShortcutRootLocalStateRecord(
	ctx context.Context,
	record *ShortcutRootRecord,
	localRows []LocalStateRow,
) (shortcutRootLocalReconcileStep, error) {
	if record == nil {
		return shortcutRootLocalReconcileStep{}, nil
	}
	normalized := normalizeShortcutRootRecord(*record)
	if shortcutRootStateAwaitsReleaseCleanup(normalized.State) {
		next, changed, err := e.finalizeShortcutRootReleaseRecord(normalized)
		return shortcutRootLocalReconcileStep{
			records:           next,
			changed:           changed,
			releaseCleanupErr: err,
		}, nil
	}
	next, keep, changed, err := e.reconcileShortcutRootRecord(ctx, *record, localRows)
	if err != nil {
		return shortcutRootLocalReconcileStep{}, err
	}
	step := shortcutRootLocalReconcileStep{changed: changed}
	if keep {
		step.records = append(step.records, next)
	}
	return step, nil
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
		ShortcutRootStateLocalRootUnavailable,
		ShortcutRootStateBlockedPath,
		ShortcutRootStateRenameAmbiguous,
		ShortcutRootStateAliasMutationBlocked,
		ShortcutRootStateDuplicateTarget:
	case ShortcutRootStateRemovedFinalDrain,
		ShortcutRootStateSamePathReplacementWaiting:
		return e.reconcileRetiringShortcutRootLocalState(record)
	case ShortcutRootStateRemovedReleasePending,
		ShortcutRootStateRemovedCleanupBlocked,
		ShortcutRootStateRemovedChildCleanupPending:
		return record, true, false, nil
	}
	observation := e.observeShortcutRootLocalState(&record)
	plan := planShortcutRootLocalObservation(record, observation)
	switch plan.Action {
	case shortcutRootLocalKeepRecord:
		return plan.Next, plan.Keep, plan.Changed, nil
	case shortcutRootLocalMaterializeRoot:
		return e.materializeShortcutRoot(record, observation.RelativePath)
	case shortcutRootLocalDropRecord,
		shortcutRootLocalMutateAlias,
		shortcutRootLocalMoveProjection:
		return plan.Next, plan.Keep, plan.Changed, nil
	case shortcutRootLocalNoop:
	}
	relativePath := observation.RelativePath
	return e.reconcileMissingMaterializedShortcutRoot(ctx, record, relativePath, localRows)
}

//nolint:gocritic // ShortcutRootRecord is treated as a value in local transition helpers.
func (e *Engine) reconcileRetiringShortcutRootLocalState(
	record ShortcutRootRecord,
) (ShortcutRootRecord, bool, bool, error) {
	plan := planRetiringShortcutRootLocalObservation(record, e.observeShortcutRootLocalState(&record))
	return plan.Next, plan.Keep, plan.Changed, nil
}

func (e *Engine) observeShortcutRootLocalState(record *ShortcutRootRecord) shortcutRootLocalObservation {
	relativePath, ok := cleanShortcutRootRelativePath(record.RelativeLocalPath)
	observation := shortcutRootLocalObservation{
		RelativePath:   relativePath,
		RelativePathOK: ok,
	}
	if !ok {
		return observation
	}
	if err := e.syncTree.ValidateNoSymlinkAncestors(relativePath); err != nil {
		observation.SymlinkErr = err
		return observation
	}
	state, err := e.syncTree.PathStateNoFollow(relativePath)
	if err != nil {
		observation.PathErr = err
		return observation
	}
	observation.PathState = state
	if !state.Exists || !state.IsDir {
		return observation
	}
	identity, identityErr := e.syncTree.IdentityNoFollow(relativePath)
	if identityErr != nil {
		observation.IdentityErr = identityErr
		return observation
	}
	observation.Identity = &identity
	return observation
}

//nolint:gocritic // ShortcutRootRecord is treated as a value in the planner-style local transition.
func (e *Engine) materializeShortcutRoot(
	record ShortcutRootRecord,
	relativePath string,
) (ShortcutRootRecord, bool, bool, error) {
	createErr := e.syncTree.MkdirAllNoFollow(relativePath, shortcutRootDirPerm)
	if createErr != nil {
		plan := planShortcutRootMaterializeResult(record, shortcutRootMaterializeResult{CreateErr: createErr})
		return shortcutRootLocalPlanResult(&plan)
	}
	identity, identityErr := e.syncTree.IdentityNoFollow(relativePath)
	if identityErr != nil {
		plan := planShortcutRootMaterializeResult(record, shortcutRootMaterializeResult{IdentityErr: identityErr})
		return shortcutRootLocalPlanResult(&plan)
	}
	plan := planShortcutRootMaterializeResult(record, shortcutRootMaterializeResult{Identity: &identity})
	return shortcutRootLocalPlanResult(&plan)
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
		return planShortcutRootUnavailable(record, candidateErr.Error()), true, true, nil
	}
	plan := planMissingMaterializedShortcutRoot(record, relativePath, candidates)
	switch plan.Action {
	case shortcutRootMissingAliasMoveProjection:
		return e.moveRemoteRenamedShortcutProjection(record, plan.FromRelativePath, plan.ToRelativePath)
	case shortcutRootMissingAliasDelete:
		result := shortcutRootAliasMutationResult{
			MutationErr: e.applyShortcutAliasMutation(ctx, plan.Mutation),
		}
		resultPlan := planMissingAliasDeleteResult(record, result)
		return shortcutRootLocalActionResult(&resultPlan)
	case shortcutRootMissingAliasRename:
		result := shortcutRootAliasMutationResult{
			MutationErr: e.applyShortcutAliasMutation(ctx, plan.Mutation),
		}
		if result.MutationErr == nil {
			identity, identityErr := e.syncTree.IdentityNoFollow(filepath.FromSlash(plan.CandidatePath))
			result.Identity = &identity
			result.IdentityErr = identityErr
		}
		resultPlan := planMissingAliasRenameResult(record, plan.CandidatePath, result)
		return shortcutRootLocalActionResult(&resultPlan)
	case shortcutRootMissingAliasRenameAmbiguous:
		return plan.Next, plan.Keep, plan.Changed, nil
	case shortcutRootMissingAliasNoop:
		return record, true, false, nil
	case shortcutRootLocalMaterializeRoot:
		return record, true, false, nil
	default:
		return record, true, false, nil
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
	moveErr := e.moveShortcutRootProjection(fromRelativePath, toRelativePath)
	if moveErr != nil {
		plan := planShortcutProjectionMoveResult(record, shortcutRootProjectionMoveResult{MoveErr: moveErr})
		return shortcutRootLocalPlanResult(&plan)
	}
	identity, identityErr := e.syncTree.IdentityNoFollow(toRelativePath)
	if identityErr != nil {
		plan := planShortcutProjectionMoveResult(record, shortcutRootProjectionMoveResult{IdentityErr: identityErr})
		return shortcutRootLocalPlanResult(&plan)
	}
	plan := planShortcutProjectionMoveResult(record, shortcutRootProjectionMoveResult{Identity: &identity})
	return shortcutRootLocalPlanResult(&plan)
}

func shortcutRootLocalPlanResult(plan *shortcutRootLocalObservationPlan) (ShortcutRootRecord, bool, bool, error) {
	if plan == nil {
		return ShortcutRootRecord{}, false, false, nil
	}
	return plan.Next, plan.Keep, plan.Changed, nil
}

func shortcutRootLocalActionResult(plan *shortcutRootLocalPlan) (ShortcutRootRecord, bool, bool, error) {
	if plan == nil {
		return ShortcutRootRecord{}, false, false, nil
	}
	return plan.Next, plan.Keep, plan.Changed, nil
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

func (e *Engine) releaseShortcutRootProjectionAfterDrain(ctx context.Context, ack ShortcutChildDrainAck) error {
	if e == nil || e.baseline == nil || ack.Ref.IsZero() {
		return nil
	}
	return e.finalizeShortcutRootReleaseByBinding(ctx, ack.Ref.bindingItemID)
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
		ShortcutRootStateLocalRootUnavailable,
		ShortcutRootStateBlockedPath,
		ShortcutRootStateRenameAmbiguous,
		ShortcutRootStateAliasMutationBlocked,
		ShortcutRootStateRemovedReleasePending,
		ShortcutRootStateRemovedChildCleanupPending,
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
		ShortcutRootStateLocalRootUnavailable,
		ShortcutRootStateBlockedPath,
		ShortcutRootStateRenameAmbiguous,
		ShortcutRootStateAliasMutationBlocked,
		ShortcutRootStateRemovedFinalDrain,
		ShortcutRootStateRemovedChildCleanupPending,
		ShortcutRootStateSamePathReplacementWaiting,
		ShortcutRootStateDuplicateTarget:
		return false
	default:
		return false
	}
}

func (e *Engine) finalizeShortcutRootReleaseByBinding(ctx context.Context, bindingItemID string) error {
	records, err := e.baseline.listShortcutRoots(ctx)
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
		next, recordChanged, recordErr := e.finalizeShortcutRootReleaseRecord(record)
		changed = changed || recordChanged
		nextRecords = append(nextRecords, next...)
		if recordErr != nil {
			cleanupErr = recordErr
		}
	}
	if changed {
		if err := e.baseline.replaceShortcutRoots(ctx, nextRecords); err != nil {
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
) ([]ShortcutRootRecord, bool, error) {
	record = normalizeShortcutRootRecord(record)
	if !shortcutRootStateAwaitsReleaseCleanup(record.State) {
		return []ShortcutRootRecord{record}, false, nil
	}
	plan := planShortcutRootReleaseCleanup(&record, e.removeShortcutRootProjection(record.RelativeLocalPath))
	return plan.Records, plan.Changed, plan.Err
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
