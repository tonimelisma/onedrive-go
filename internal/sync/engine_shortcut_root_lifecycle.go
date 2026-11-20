package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"

	"github.com/tonimelisma/onedrive-go/internal/namekey"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncshortcut"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// This file is the shortcut-root local lifecycle shell. It gathers filesystem
// and store facts, executes Graph/filesystem effects, and persists planner
// outputs. Durable shortcut policy decisions stay in shortcut_root_planner_*.
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
	nextRecords := make([]syncshortcut.ShortcutRootRecord, 0, len(records))
	var newlyBlocked []shortcutRootCleanupBlockedReport
	for i := range records {
		step, recordErr := e.reconcileShortcutRootLocalStateRecord(ctx, &records[i], localRows)
		if recordErr != nil {
			return false, recordErr
		}
		nextRecords = append(nextRecords, step.records...)
		changed = changed || step.changed
		if step.releaseCleanupErr == nil {
			continue
		}
		// A blocked cleanup is durable state: it is persisted below, `status`
		// shows it, and it is retried here every pass. It is never returned
		// as an error, because that would fail every one-shot run and end
		// the watch remote observer, stalling unrelated remote sync over a
		// condition only the user can clear. It is reported once, when the
		// record first enters the blocked state (or its reason changes);
		// an unchanged repeat stays at Debug.
		if !step.changed {
			e.logger.Debug("shortcut root release cleanup still blocked",
				slog.String("path", records[i].RelativeLocalPath),
				slog.Any("error", step.releaseCleanupErr),
			)
			continue
		}
		newlyBlocked = append(newlyBlocked, shortcutRootCleanupBlockedReport{
			relativeLocalPath: records[i].RelativeLocalPath,
			err:               step.releaseCleanupErr,
		})
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
	for i := range newlyBlocked {
		e.logger.Warn("Could not remove the local copy of removed shared folder "+
			newlyBlocked[i].relativeLocalPath+
			"; move or delete what is in the way and the next sync finishes the cleanup",
			slog.Any("error", newlyBlocked[i].err),
		)
	}
	return true, nil
}

// shortcutRootCleanupBlockedReport is one record that entered the
// cleanup-blocked state in this pass, reported after the state is persisted.
type shortcutRootCleanupBlockedReport struct {
	relativeLocalPath string
	err               error
}

type shortcutRootLocalReconcileStep struct {
	records           []syncshortcut.ShortcutRootRecord
	changed           bool
	releaseCleanupErr error
}

func (e *Engine) reconcileShortcutRootLocalStateRecord(
	ctx context.Context,
	record *syncshortcut.ShortcutRootRecord,
	localRows []syncdomain.LocalStateRow,
) (shortcutRootLocalReconcileStep, error) {
	if record == nil {
		return shortcutRootLocalReconcileStep{}, nil
	}
	normalized := syncshortcut.NormalizeShortcutRootRecord(record)
	if syncshortcut.ShortcutRootStateAwaitsReleaseCleanup(normalized.State) {
		next, changed, err := e.finalizeShortcutRootReleaseRecord(&normalized)
		return shortcutRootLocalReconcileStep{
			records:           next,
			changed:           changed,
			releaseCleanupErr: err,
		}, nil
	}
	next, keep, changed, err := e.reconcileShortcutRootRecord(ctx, record, localRows)
	if err != nil {
		return shortcutRootLocalReconcileStep{}, err
	}
	step := shortcutRootLocalReconcileStep{changed: changed}
	if keep {
		step.records = append(step.records, next)
	}
	return step, nil
}

func (e *Engine) reconcileShortcutRootRecord(
	ctx context.Context,
	record *syncshortcut.ShortcutRootRecord,
	localRows []syncdomain.LocalStateRow,
) (syncshortcut.ShortcutRootRecord, bool, bool, error) {
	if record == nil {
		return syncshortcut.ShortcutRootRecord{}, false, false, nil
	}
	normalized := syncshortcut.NormalizeShortcutRootRecord(record)
	switch normalized.State {
	case "",
		syncshortcut.ShortcutRootStateActive,
		syncshortcut.ShortcutRootStateTargetUnavailable,
		syncshortcut.ShortcutRootStateLocalRootUnavailable,
		syncshortcut.ShortcutRootStateBlockedPath,
		syncshortcut.ShortcutRootStateRenameAmbiguous,
		syncshortcut.ShortcutRootStateAliasMutationBlocked,
		syncshortcut.ShortcutRootStateDuplicateTarget:
	case syncshortcut.ShortcutRootStateRemovedFinalDrain,
		syncshortcut.ShortcutRootStateSamePathReplacementWaiting:
		return e.reconcileRetiringShortcutRootLocalState(&normalized)
	case syncshortcut.ShortcutRootStateRemovedReleasePending,
		syncshortcut.ShortcutRootStateRemovedCleanupBlocked,
		syncshortcut.ShortcutRootStateRemovedChildCleanupPending:
		return normalized, true, false, nil
	}
	observation := e.observeShortcutRootLocalState(&normalized)
	plan := syncshortcut.PlanShortcutRootLocalObservation(&normalized, &observation)
	switch plan.Action {
	case syncshortcut.ShortcutRootLocalKeepRecord:
		return plan.Next, plan.Keep, plan.Changed, nil
	case syncshortcut.ShortcutRootLocalMaterializeRoot:
		return e.materializeShortcutRoot(&normalized, observation.RelativePath)
	case syncshortcut.ShortcutRootLocalDropRecord,
		syncshortcut.ShortcutRootLocalMutateAlias,
		syncshortcut.ShortcutRootLocalMoveProjection:
		return plan.Next, plan.Keep, plan.Changed, nil
	case syncshortcut.ShortcutRootLocalNoop:
	}
	relativePath := observation.RelativePath
	return e.reconcileMissingMaterializedShortcutRoot(ctx, &normalized, relativePath, localRows)
}

func (e *Engine) reconcileRetiringShortcutRootLocalState(
	record *syncshortcut.ShortcutRootRecord,
) (syncshortcut.ShortcutRootRecord, bool, bool, error) {
	if record == nil {
		return syncshortcut.ShortcutRootRecord{}, false, false, nil
	}
	observation := e.observeShortcutRootLocalState(record)
	plan := syncshortcut.PlanRetiringShortcutRootLocalObservation(record, &observation)
	return plan.Next, plan.Keep, plan.Changed, nil
}

func (e *Engine) observeShortcutRootLocalState(record *syncshortcut.ShortcutRootRecord) syncshortcut.ShortcutRootLocalObservation {
	relativePath, ok := syncshortcut.CleanShortcutRootRelativePath(record.RelativeLocalPath)
	observation := syncshortcut.ShortcutRootLocalObservation{
		RelativePath:   relativePath,
		RelativePathOK: ok,
	}
	if !ok {
		return observation
	}
	onDisk := e.shortcutRootOnDisk(relativePath)
	if err := e.syncTree.ValidateNoSymlinkAncestors(onDisk); err != nil {
		observation.SymlinkErr = err
		return observation
	}
	state, err := e.syncTree.PathStateNoFollow(onDisk)
	if err != nil {
		observation.PathErr = err
		return observation
	}
	observation.PathState = state
	if !state.Exists || !state.IsDir {
		return observation
	}
	identity, identityErr := e.syncTree.IdentityNoFollow(onDisk)
	if identityErr != nil {
		observation.IdentityErr = identityErr
		return observation
	}
	observation.Identity = &identity
	return observation
}

func (e *Engine) materializeShortcutRoot(
	record *syncshortcut.ShortcutRootRecord,
	relativePath string,
) (syncshortcut.ShortcutRootRecord, bool, bool, error) {
	if record == nil {
		return syncshortcut.ShortcutRootRecord{}, false, false, nil
	}
	// Existing parents are addressed as spelled on disk; the folder itself,
	// new, gets the stored name. It and any parent it needs are folders in
	// the user's tree, like any other the sync tree creates (R-5.10.2).
	onDisk := e.shortcutRootOnDisk(relativePath)
	createErr := e.syncTree.MkdirAllNoFollow(onDisk)
	if createErr != nil {
		plan := syncshortcut.PlanShortcutRootMaterializeResult(record, syncshortcut.ShortcutRootMaterializeResult{CreateErr: createErr})
		return syncshortcut.ShortcutRootLocalPlanResult(&plan)
	}
	identity, identityErr := e.syncTree.IdentityNoFollow(onDisk)
	if identityErr != nil {
		plan := syncshortcut.PlanShortcutRootMaterializeResult(record, syncshortcut.ShortcutRootMaterializeResult{IdentityErr: identityErr})
		return syncshortcut.ShortcutRootLocalPlanResult(&plan)
	}
	plan := syncshortcut.PlanShortcutRootMaterializeResult(record, syncshortcut.ShortcutRootMaterializeResult{Identity: &identity})
	return syncshortcut.ShortcutRootLocalPlanResult(&plan)
}

func (e *Engine) reconcileMissingMaterializedShortcutRoot(
	ctx context.Context,
	record *syncshortcut.ShortcutRootRecord,
	relativePath string,
	localRows []syncdomain.LocalStateRow,
) (syncshortcut.ShortcutRootRecord, bool, bool, error) {
	if record == nil || record.LocalRootIdentity == nil {
		return syncshortcut.ShortcutRootRecord{}, false, false, nil
	}
	candidates, candidateErr := e.shortcutRootIdentityCandidates(relativePath, *record.LocalRootIdentity, localRows)
	observation := syncshortcut.ShortcutRootLocalObservation{
		RelativePath: relativePath,
		Candidates:   candidates,
		CandidateErr: candidateErr,
	}
	plan := syncshortcut.PlanMissingMaterializedShortcutRootObservation(record, &observation)
	return e.executeMissingMaterializedShortcutRootPlan(ctx, record, &plan)
}

func (e *Engine) executeMissingMaterializedShortcutRootPlan(
	ctx context.Context,
	record *syncshortcut.ShortcutRootRecord,
	plan *syncshortcut.ShortcutRootLocalPlan,
) (syncshortcut.ShortcutRootRecord, bool, bool, error) {
	if record == nil || plan == nil {
		return syncshortcut.ShortcutRootRecord{}, false, false, nil
	}
	switch plan.Action {
	case syncshortcut.ShortcutRootMissingAliasMoveProjection:
		return e.moveRemoteRenamedShortcutProjection(record, plan.FromRelativePath, plan.ToRelativePath)
	case syncshortcut.ShortcutRootMissingAliasDelete:
		result := syncshortcut.ShortcutRootAliasMutationResult{
			MutationErr: e.applyShortcutAliasMutation(ctx, plan.Mutation),
		}
		resultPlan := syncshortcut.PlanMissingAliasDeleteResult(record, result)
		return syncshortcut.ShortcutRootLocalActionResult(&resultPlan)
	case syncshortcut.ShortcutRootMissingAliasRename:
		result := syncshortcut.ShortcutRootAliasMutationResult{
			MutationErr: e.applyShortcutAliasMutation(ctx, plan.Mutation),
		}
		if result.MutationErr == nil {
			identity, identityErr := e.syncTree.IdentityNoFollow(e.shortcutRootOnDisk(plan.CandidatePath))
			result.Identity = &identity
			result.IdentityErr = identityErr
		}
		resultPlan := syncshortcut.PlanMissingAliasRenameResult(record, plan.CandidatePath, result)
		return syncshortcut.ShortcutRootLocalActionResult(&resultPlan)
	case syncshortcut.ShortcutRootMissingAliasRenameAmbiguous:
		return plan.Next, plan.Keep, plan.Changed, nil
	case syncshortcut.ShortcutRootMissingAliasNoop:
		return *record, true, false, nil
	case syncshortcut.ShortcutRootLocalMaterializeRoot:
		return *record, true, false, nil
	default:
		return *record, true, false, nil
	}
}

func (e *Engine) shortcutRootIdentityCandidates(
	relativePath string,
	identity synctree.FileIdentity,
	localRows []syncdomain.LocalStateRow,
) ([]string, error) {
	candidates := syncshortcut.ShortcutRootIdentityCandidatesFromLocalState(relativePath, identity, localRows)
	liveCandidates, err := e.shortcutRootIdentityCandidatesFromFilesystem(relativePath, identity)
	if err != nil {
		return nil, err
	}
	candidates = append(candidates, liveCandidates...)
	return syncshortcut.UniqueSortedShortcutRootCandidates(candidates), nil
}

func (e *Engine) shortcutRootIdentityCandidatesFromFilesystem(
	relativePath string,
	identity synctree.FileIdentity,
) ([]string, error) {
	if e == nil || e.syncTree == nil {
		return nil, nil
	}
	normalizedCurrent := syncshortcut.NormalizedProtectedRootPath(relativePath)
	parent := path.Dir(normalizedCurrent)
	if parent == "." {
		parent = ""
	}
	parentOnDisk := ""
	if parent != "" {
		parentOnDisk = e.shortcutRootOnDisk(parent)
	}
	entries, err := e.syncTree.ReadDir(parentOnDisk)
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
			parentOnDisk,
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
	return syncshortcut.UniqueSortedShortcutRootCandidates(candidates), nil
}

// shortcutRootIdentityCandidateFromFilesystemEntry checks one entry of the
// alias's parent, listed at parentOnDisk. A candidate is returned as a stored
// path (NFC, under parent), the form records and the alias's own path use:
// the alias itself, listed under an NFD name, is not a rename of itself
// (R-2.13.2).
func (e *Engine) shortcutRootIdentityCandidateFromFilesystemEntry(
	parent string,
	parentOnDisk string,
	normalizedCurrent string,
	entryName string,
	identity synctree.FileIdentity,
) (string, bool, error) {
	candidate := namekey.Stored(entryName)
	if parent != "" {
		candidate = path.Join(parent, candidate)
	}
	if candidate == normalizedCurrent {
		return "", false, nil
	}
	onDisk := entryName
	if parentOnDisk != "" {
		onDisk = filepath.Join(parentOnDisk, entryName)
	}
	state, err := e.syncTree.PathStateNoFollow(onDisk)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("stat shortcut alias rename candidate: %w", err)
	}
	if !state.Exists || !state.IsDir {
		return "", false, nil
	}
	candidateIdentity, err := e.syncTree.IdentityNoFollow(onDisk)
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

func (e *Engine) moveRemoteRenamedShortcutProjection(
	record *syncshortcut.ShortcutRootRecord,
	fromRelativePath string,
	toRelativePath string,
) (syncshortcut.ShortcutRootRecord, bool, bool, error) {
	if record == nil {
		return syncshortcut.ShortcutRootRecord{}, false, false, nil
	}
	moveErr := e.moveShortcutRootProjection(fromRelativePath, toRelativePath)
	if moveErr != nil {
		plan := syncshortcut.PlanShortcutProjectionMoveResult(record, syncshortcut.ShortcutRootProjectionMoveResult{MoveErr: moveErr})
		return syncshortcut.ShortcutRootLocalPlanResult(&plan)
	}
	identity, identityErr := e.syncTree.IdentityNoFollow(e.shortcutRootOnDisk(toRelativePath))
	if identityErr != nil {
		plan := syncshortcut.PlanShortcutProjectionMoveResult(record, syncshortcut.ShortcutRootProjectionMoveResult{IdentityErr: identityErr})
		return syncshortcut.ShortcutRootLocalPlanResult(&plan)
	}
	plan := syncshortcut.PlanShortcutProjectionMoveResult(record, syncshortcut.ShortcutRootProjectionMoveResult{Identity: &identity})
	return syncshortcut.ShortcutRootLocalPlanResult(&plan)
}

func (e *Engine) moveShortcutRootProjection(fromRelativePath string, toRelativePath string) error {
	if fromRelativePath == "" || toRelativePath == "" || fromRelativePath == toRelativePath {
		return nil
	}
	// Both endpoints as spelled on disk: the source, and the destination's
	// existing parents (and an existing destination, a case-only rename).
	fromRelativePath = e.shortcutRootOnDisk(fromRelativePath)
	toRelativePath = e.shortcutRootOnDisk(toRelativePath)
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
	if err := e.syncTree.MkdirAllNoFollow(filepath.Dir(toRelativePath)); err != nil {
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
	err := e.syncTree.RenameWithTemporarySibling(
		fromRelativePath,
		toRelativePath,
		syncshortcut.ShortcutRootCaseRenameTempInfix,
		syncshortcut.ShortcutRootCaseRenameAttempts,
	)
	switch {
	case errors.Is(err, synctree.ErrSourceNotRemoved):
		// Reached only by a file, which validateShortcutRootProjectionMove refuses; a folder is never linked.
		e.logger.Warn("Moved shortcut alias projection "+toRelativePath+", but could not remove a name it was moved from",
			slog.String("path", toRelativePath), slog.Any("error", err))
	case err != nil:
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

// shortcutRootOnDisk returns a stored (NFC) shortcut-root path as the
// filesystem spells it, for every operation on an entry that may already
// exist (R-2.13.2): on a byte-exact filesystem an alias folder or a parent
// with an NFD name on disk is missing under its stored spelling, and reading
// it as missing deleted the alias online or materialized an empty duplicate
// beside it.
func (e *Engine) shortcutRootOnDisk(storedPath string) string {
	return e.syncTree.Spelling(filepath.FromSlash(storedPath))
}

func (e *Engine) releaseShortcutRootProjectionAfterDrain(ctx context.Context, ack syncshortcut.ShortcutChildDrainAck) error {
	if e == nil || e.baseline == nil || ack.Ref.IsZero() {
		return nil
	}
	return e.finalizeShortcutRootReleaseByBinding(ctx, ack.Ref)
}

func (e *Engine) finalizeShortcutRootReleaseByBinding(ctx context.Context, ref syncshortcut.ShortcutChildAckRef) error {
	records, err := e.baseline.ListShortcutRoots(ctx)
	if err != nil {
		return fmt.Errorf("sync: read shortcut roots before child drain release: %w", err)
	}
	changed := false
	nextRecords := make([]syncshortcut.ShortcutRootRecord, 0, len(records))
	var cleanupErr error
	for i := range records {
		record := syncshortcut.NormalizeShortcutRootRecord(&records[i])
		if !ref.Matches(&record) || !syncshortcut.ShortcutRootStateAwaitsReleaseCleanup(record.State) {
			nextRecords = append(nextRecords, record)
			continue
		}
		next, recordChanged, recordErr := e.finalizeShortcutRootReleaseRecord(&record)
		changed = changed || recordChanged
		nextRecords = append(nextRecords, next...)
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

func (e *Engine) finalizeShortcutRootReleaseRecord(
	record *syncshortcut.ShortcutRootRecord,
) ([]syncshortcut.ShortcutRootRecord, bool, error) {
	if record == nil {
		return nil, false, nil
	}
	normalized := syncshortcut.NormalizeShortcutRootRecord(record)
	if !syncshortcut.ShortcutRootStateAwaitsReleaseCleanup(normalized.State) {
		return []syncshortcut.ShortcutRootRecord{normalized}, false, nil
	}
	result := e.removeShortcutRootProjection(normalized.RelativeLocalPath)
	if result.NotEmpty {
		// The child released its tree before acknowledging, so something
		// arrived since; the child, which owns that content, runs again.
		e.logger.Debug("removed shared folder's local folder is not empty; its final sync runs again",
			slog.String("path", normalized.RelativeLocalPath))
	}
	plan := syncshortcut.PlanShortcutRootReleaseCleanup(&normalized, result)
	return plan.Records, plan.Changed, plan.Err
}

// removeShortcutRootProjection removes a removed shared folder's alias root
// once the child's final drain has taken its content out by the local delete
// rule (engine_shortcut_release.go). The root goes only as an empty
// directory: this engine never walks a tree it does not sync, and a root that
// still holds something is the child's to release.
func (e *Engine) removeShortcutRootProjection(relativeLocalPath string) syncshortcut.ShortcutRootReleaseResult {
	relativePath, ok := syncshortcut.CleanShortcutRootRelativePath(relativeLocalPath)
	if !ok {
		return syncshortcut.ShortcutRootReleaseResult{
			Err: fmt.Errorf("%w: the shared folder's local path leaves the sync folder", synctree.ErrUnsafePath),
		}
	}
	relativePath = e.shortcutRootOnDisk(relativePath)
	if err := e.syncTree.ValidateNoSymlinkAncestors(relativePath); err != nil {
		return syncshortcut.ShortcutRootReleaseResult{Err: fmt.Errorf("checking the removed shared folder's local folder: %w", err)}
	}
	state, err := e.syncTree.PathStateNoFollow(relativePath)
	if err != nil {
		return syncshortcut.ShortcutRootReleaseResult{Err: fmt.Errorf("reading the removed shared folder's local folder: %w", err)}
	}
	if !state.Exists {
		return syncshortcut.ShortcutRootReleaseResult{}
	}
	if !state.IsDir {
		return syncshortcut.ShortcutRootReleaseResult{
			Err: fmt.Errorf("%w: the shared folder's local path is not a directory", synctree.ErrUnsafePath),
		}
	}
	if notEmpty, err := e.shortcutRootHoldsEntries(relativePath); err != nil || notEmpty {
		return syncshortcut.ShortcutRootReleaseResult{NotEmpty: notEmpty, Err: err}
	}
	removeErr := e.syncTree.RemoveEmptyDirNoFollow(relativePath)
	if removeErr == nil {
		return syncshortcut.ShortcutRootReleaseResult{}
	}
	// An entry that arrived after the check refuses the removal; that is the
	// child's again, not a blocked cleanup.
	if notEmpty, err := e.shortcutRootHoldsEntries(relativePath); err == nil && notEmpty {
		return syncshortcut.ShortcutRootReleaseResult{NotEmpty: true}
	}

	return syncshortcut.ShortcutRootReleaseResult{Err: fmt.Errorf("removing the removed shared folder's local folder: %w", removeErr)}
}

// shortcutRootHoldsEntries reports whether the alias root at its on-disk
// spelling holds any entry.
func (e *Engine) shortcutRootHoldsEntries(onDisk string) (bool, error) {
	empty, err := e.syncTree.DirEmptyNoFollow(onDisk)
	if err != nil {
		return false, fmt.Errorf("reading the removed shared folder's local folder: %w", err)
	}

	return !empty, nil
}
