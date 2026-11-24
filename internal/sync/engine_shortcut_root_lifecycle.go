package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/namekey"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncshortcut"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// shortcutRoots owns the shortcut roots below this mount: their rows in the
// store, their local projections in the sync tree, and the child-work
// snapshot it publishes to the multisync orchestrator. The watch loop, a
// one-shot pass and the orchestrator's ack handle (on its own goroutine) all
// enter it, so it touches only the store, the protected-root set (which
// guards itself), the sync tree and values fixed at construction.
type shortcutRoots struct {
	store         *syncstore.SyncStore
	syncTree      *synctree.Root
	syncRoot      string
	roots         *protectedRootSet
	items         syncdomain.ItemClient
	driveID       driveid.ID
	namespaceID   string
	sink          syncshortcut.ShortcutChildWorkSink
	contentFilter syncdomain.ContentFilterConfig
	logger        *slog.Logger
}

// shortcutRoots is the engine's shortcut roots as configured now. A run takes
// them once (flowInputs); the ack handle takes them per acknowledgement.
func (e *Engine) shortcutRoots() *shortcutRoots {
	return &shortcutRoots{
		store:         e.baseline,
		syncTree:      e.syncTree,
		syncRoot:      e.syncRoot,
		roots:         e.protectedRoots,
		items:         e.itemsClient,
		driveID:       e.driveID,
		namespaceID:   e.shortcutNamespaceID,
		sink:          e.shortcutChildWorkSink,
		contentFilter: e.contentFilter,
		logger:        e.logger,
	}
}

// This file is the shortcut-root local lifecycle shell. It gathers filesystem
// and store facts, executes Graph/filesystem effects, and persists planner
// outputs. Durable shortcut policy decisions stay in shortcut_root_planner_*.
func (s *shortcutRoots) reconcileShortcutRootLocalState(ctx context.Context) (bool, error) {
	if s == nil || s.store == nil {
		return false, nil
	}
	records, err := s.store.ListShortcutRoots(ctx)
	if err != nil {
		return false, fmt.Errorf("sync: read shortcut roots for local lifecycle: %w", err)
	}
	localRows, err := s.store.ListLocalState(ctx)
	if err != nil {
		return false, fmt.Errorf("sync: read local_state for shortcut root local lifecycle: %w", err)
	}
	changed := false
	nextRecords := make([]syncshortcut.ShortcutRootRecord, 0, len(records))
	var newlyBlocked []shortcutRootCleanupBlockedReport
	for i := range records {
		step, recordErr := s.reconcileShortcutRootLocalStateRecord(ctx, &records[i], localRows)
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
			s.logger.Debug("shortcut root release cleanup still blocked",
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
	if err := s.store.ReplaceShortcutRoots(ctx, nextRecords); err != nil {
		return false, fmt.Errorf("sync: persist shortcut root local lifecycle: %w", err)
	}
	if err := s.roots.refresh(ctx); err != nil {
		return false, fmt.Errorf("sync: refresh shortcut protected roots: %w", err)
	}
	for i := range newlyBlocked {
		s.logger.Warn("Could not remove the local copy of removed shared folder "+
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

func (s *shortcutRoots) reconcileShortcutRootLocalStateRecord(
	ctx context.Context,
	record *syncshortcut.ShortcutRootRecord,
	localRows []syncdomain.LocalStateRow,
) (shortcutRootLocalReconcileStep, error) {
	if record == nil {
		return shortcutRootLocalReconcileStep{}, nil
	}
	normalized := syncshortcut.NormalizeShortcutRootRecord(record)
	// A removed shortcut the user's filters exclude has no child run to
	// drain it; it is finished here, its folder left as it is.
	if syncshortcut.ShortcutRootExcludedByContentFilter(s.contentFilter, normalized.RelativeLocalPath) {
		if next, changed := syncshortcut.PlanExcludedRetiringShortcutRoot(&normalized); changed {
			return shortcutRootLocalReconcileStep{records: next, changed: true}, nil
		}
	}
	if syncshortcut.ShortcutRootStateAwaitsReleaseCleanup(normalized.State) {
		next, changed, err := s.finalizeShortcutRootReleaseRecord(&normalized)
		return shortcutRootLocalReconcileStep{
			records:           next,
			changed:           changed,
			releaseCleanupErr: err,
		}, nil
	}
	next, keep, changed, err := s.reconcileShortcutRootRecord(ctx, record, localRows)
	if err != nil {
		return shortcutRootLocalReconcileStep{}, err
	}
	step := shortcutRootLocalReconcileStep{changed: changed}
	if keep {
		step.records = append(step.records, next)
	}
	return step, nil
}

func (s *shortcutRoots) reconcileShortcutRootRecord(
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
		return s.reconcileRetiringShortcutRootLocalState(&normalized)
	case syncshortcut.ShortcutRootStateRemovedReleasePending,
		syncshortcut.ShortcutRootStateRemovedCleanupBlocked,
		syncshortcut.ShortcutRootStateRemovedChildCleanupPending:
		return normalized, true, false, nil
	}
	// A shortcut the user's filters exclude is not touched: no folder is
	// made for it, a folder already there is left alone, and its alias is
	// never renamed or deleted on OneDrive from what the local side shows.
	if syncshortcut.ShortcutRootExcludedByContentFilter(s.contentFilter, normalized.RelativeLocalPath) {
		return normalized, true, false, nil
	}
	observation := s.observeShortcutRootLocalState(&normalized)
	plan := syncshortcut.PlanShortcutRootLocalObservation(&normalized, &observation)
	switch plan.Action {
	case syncshortcut.ShortcutRootLocalKeepRecord:
		return plan.Next, plan.Keep, plan.Changed, nil
	case syncshortcut.ShortcutRootLocalMaterializeRoot:
		return s.materializeShortcutRoot(&normalized, observation.RelativePath)
	case syncshortcut.ShortcutRootLocalDropRecord,
		syncshortcut.ShortcutRootLocalMutateAlias,
		syncshortcut.ShortcutRootLocalMoveProjection:
		return plan.Next, plan.Keep, plan.Changed, nil
	case syncshortcut.ShortcutRootLocalNoop:
	}
	relativePath := observation.RelativePath
	return s.reconcileMissingMaterializedShortcutRoot(ctx, &normalized, relativePath, localRows)
}

func (s *shortcutRoots) reconcileRetiringShortcutRootLocalState(
	record *syncshortcut.ShortcutRootRecord,
) (syncshortcut.ShortcutRootRecord, bool, bool, error) {
	if record == nil {
		return syncshortcut.ShortcutRootRecord{}, false, false, nil
	}
	observation := s.observeShortcutRootLocalState(record)
	plan := syncshortcut.PlanRetiringShortcutRootLocalObservation(record, &observation)
	return plan.Next, plan.Keep, plan.Changed, nil
}

func (s *shortcutRoots) observeShortcutRootLocalState(record *syncshortcut.ShortcutRootRecord) syncshortcut.ShortcutRootLocalObservation {
	relativePath, ok := syncshortcut.CleanShortcutRootRelativePath(record.RelativeLocalPath)
	observation := syncshortcut.ShortcutRootLocalObservation{
		RelativePath:   relativePath,
		RelativePathOK: ok,
	}
	if !ok {
		return observation
	}
	onDisk := s.shortcutRootOnDisk(relativePath)
	if err := s.syncTree.ValidateNoSymlinkAncestors(onDisk); err != nil {
		observation.SymlinkErr = err
		return observation
	}
	state, err := s.syncTree.PathStateNoFollow(onDisk)
	if err != nil {
		observation.PathErr = err
		return observation
	}
	if state.Exists {
		// On a volume that folds case the lstat finds a case variant of the
		// alias, which is another folder, never the alias's (R-2.12.10).
		holds, holdsErr := s.syncTree.HoldsName(onDisk)
		if holdsErr != nil {
			observation.PathErr = holdsErr
			return observation
		}
		if !holds {
			return observation
		}
	}
	observation.PathState = state
	if !state.Exists || !state.IsDir {
		return observation
	}
	identity, identityErr := s.syncTree.IdentityNoFollow(onDisk)
	if identityErr != nil {
		observation.IdentityErr = identityErr
		return observation
	}
	observation.Identity = &identity
	return observation
}

func (s *shortcutRoots) materializeShortcutRoot(
	record *syncshortcut.ShortcutRootRecord,
	relativePath string,
) (syncshortcut.ShortcutRootRecord, bool, bool, error) {
	if record == nil {
		return syncshortcut.ShortcutRootRecord{}, false, false, nil
	}
	// Existing parents are addressed as spelled on disk; the folder itself,
	// new, gets the stored name. It and any parent it needs are folders in
	// the user's tree, like any other the sync tree creates (R-5.10.2). The
	// folder adopts no entry it did not make (R-2.12.10): another entry at
	// the name blocks the alias's path (synctree.ErrTargetExists).
	onDisk, createErr := s.syncTree.MkdirExact(s.shortcutRootOnDisk(relativePath))
	if createErr != nil {
		plan := syncshortcut.PlanShortcutRootMaterializeResult(record, syncshortcut.ShortcutRootMaterializeResult{CreateErr: createErr})
		return syncshortcut.ShortcutRootLocalPlanResult(&plan)
	}
	identity, identityErr := s.syncTree.IdentityNoFollow(onDisk)
	if identityErr != nil {
		plan := syncshortcut.PlanShortcutRootMaterializeResult(record, syncshortcut.ShortcutRootMaterializeResult{IdentityErr: identityErr})
		return syncshortcut.ShortcutRootLocalPlanResult(&plan)
	}
	plan := syncshortcut.PlanShortcutRootMaterializeResult(record, syncshortcut.ShortcutRootMaterializeResult{Identity: &identity})
	return syncshortcut.ShortcutRootLocalPlanResult(&plan)
}

func (s *shortcutRoots) reconcileMissingMaterializedShortcutRoot(
	ctx context.Context,
	record *syncshortcut.ShortcutRootRecord,
	relativePath string,
	localRows []syncdomain.LocalStateRow,
) (syncshortcut.ShortcutRootRecord, bool, bool, error) {
	if record == nil || record.LocalRootIdentity == nil {
		return syncshortcut.ShortcutRootRecord{}, false, false, nil
	}
	candidates, candidateErr := s.shortcutRootIdentityCandidates(relativePath, *record.LocalRootIdentity, localRows)
	observation := syncshortcut.ShortcutRootLocalObservation{
		RelativePath: relativePath,
		Candidates:   candidates,
		CandidateErr: candidateErr,
	}
	plan := syncshortcut.PlanMissingMaterializedShortcutRootObservation(record, &observation)
	return s.executeMissingMaterializedShortcutRootPlan(ctx, record, &plan)
}

func (s *shortcutRoots) executeMissingMaterializedShortcutRootPlan(
	ctx context.Context,
	record *syncshortcut.ShortcutRootRecord,
	plan *syncshortcut.ShortcutRootLocalPlan,
) (syncshortcut.ShortcutRootRecord, bool, bool, error) {
	if record == nil || plan == nil {
		return syncshortcut.ShortcutRootRecord{}, false, false, nil
	}
	switch plan.Action {
	case syncshortcut.ShortcutRootMissingAliasMoveProjection:
		return s.moveRemoteRenamedShortcutProjection(record, plan.FromRelativePath, plan.ToRelativePath)
	case syncshortcut.ShortcutRootMissingAliasDelete:
		result := syncshortcut.ShortcutRootAliasMutationResult{
			MutationErr: s.applyShortcutAliasMutation(ctx, plan.Mutation),
		}
		resultPlan := syncshortcut.PlanMissingAliasDeleteResult(record, result)
		return syncshortcut.ShortcutRootLocalActionResult(&resultPlan)
	case syncshortcut.ShortcutRootMissingAliasRename:
		result := syncshortcut.ShortcutRootAliasMutationResult{
			MutationErr: s.applyShortcutAliasMutation(ctx, plan.Mutation),
		}
		if result.MutationErr == nil {
			identity, identityErr := s.syncTree.IdentityNoFollow(s.shortcutRootOnDisk(plan.CandidatePath))
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

func (s *shortcutRoots) shortcutRootIdentityCandidates(
	relativePath string,
	identity synctree.FileIdentity,
	localRows []syncdomain.LocalStateRow,
) ([]string, error) {
	candidates := syncshortcut.ShortcutRootIdentityCandidatesFromLocalState(relativePath, identity, localRows)
	liveCandidates, err := s.shortcutRootIdentityCandidatesFromFilesystem(relativePath, identity)
	if err != nil {
		return nil, err
	}
	candidates = append(candidates, liveCandidates...)
	return syncshortcut.UniqueSortedShortcutRootCandidates(candidates), nil
}

func (s *shortcutRoots) shortcutRootIdentityCandidatesFromFilesystem(
	relativePath string,
	identity synctree.FileIdentity,
) ([]string, error) {
	if s == nil || s.syncTree == nil {
		return nil, nil
	}
	normalizedCurrent := syncshortcut.NormalizedProtectedRootPath(relativePath)
	parent := path.Dir(normalizedCurrent)
	if parent == "." {
		parent = ""
	}
	parentOnDisk := ""
	if parent != "" {
		parentOnDisk = s.shortcutRootOnDisk(parent)
	}
	entries, err := s.syncTree.ReadDir(parentOnDisk)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read shortcut alias parent directory: %w", err)
	}
	candidates := make([]string, 0)
	for _, entry := range entries {
		candidate, found, err := s.shortcutRootIdentityCandidateFromFilesystemEntry(
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
func (s *shortcutRoots) shortcutRootIdentityCandidateFromFilesystemEntry(
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
	state, err := s.syncTree.PathStateNoFollow(onDisk)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("stat shortcut alias rename candidate: %w", err)
	}
	if !state.Exists || !state.IsDir {
		return "", false, nil
	}
	candidateIdentity, err := s.syncTree.IdentityNoFollow(onDisk)
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

func (s *shortcutRoots) moveRemoteRenamedShortcutProjection(
	record *syncshortcut.ShortcutRootRecord,
	fromRelativePath string,
	toRelativePath string,
) (syncshortcut.ShortcutRootRecord, bool, bool, error) {
	if record == nil {
		return syncshortcut.ShortcutRootRecord{}, false, false, nil
	}
	moveErr := s.moveShortcutRootProjection(fromRelativePath, toRelativePath)
	if moveErr != nil {
		plan := syncshortcut.PlanShortcutProjectionMoveResult(record, syncshortcut.ShortcutRootProjectionMoveResult{MoveErr: moveErr})
		return syncshortcut.ShortcutRootLocalPlanResult(&plan)
	}
	identity, identityErr := s.syncTree.IdentityNoFollow(s.shortcutRootOnDisk(toRelativePath))
	if identityErr != nil {
		plan := syncshortcut.PlanShortcutProjectionMoveResult(record, syncshortcut.ShortcutRootProjectionMoveResult{IdentityErr: identityErr})
		return syncshortcut.ShortcutRootLocalPlanResult(&plan)
	}
	plan := syncshortcut.PlanShortcutProjectionMoveResult(record, syncshortcut.ShortcutRootProjectionMoveResult{Identity: &identity})
	return syncshortcut.ShortcutRootLocalPlanResult(&plan)
}

func (s *shortcutRoots) moveShortcutRootProjection(fromRelativePath string, toRelativePath string) error {
	if fromRelativePath == "" || toRelativePath == "" || fromRelativePath == toRelativePath {
		return nil
	}
	// Both endpoints as spelled on disk: the source, and the destination's
	// existing parents (and an existing destination, a case-only rename).
	fromRelativePath = s.shortcutRootOnDisk(fromRelativePath)
	toRelativePath = s.shortcutRootOnDisk(toRelativePath)
	if err := s.validateShortcutRootProjectionMove(fromRelativePath, toRelativePath); err != nil {
		return err
	}
	toState, err := s.syncTree.PathStateNoFollow(toRelativePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stating current shortcut alias projection: %w", err)
	}
	if toState.Exists {
		moved, err := s.moveShortcutRootProjectionOntoExistingTarget(fromRelativePath, toRelativePath)
		if err != nil {
			return err
		}
		if moved {
			return nil
		}
		return fmt.Errorf("%w: current shortcut alias projection already exists", synctree.ErrUnsafePath)
	}
	if err := s.syncTree.MkdirAllNoFollow(filepath.Dir(toRelativePath)); err != nil {
		return fmt.Errorf("creating current shortcut alias parent: %w", err)
	}
	if err := s.syncTree.Rename(fromRelativePath, toRelativePath); err != nil {
		return fmt.Errorf("moving shortcut alias projection: %w", err)
	}
	return nil
}

func (s *shortcutRoots) moveShortcutRootProjectionOntoExistingTarget(
	fromRelativePath string,
	toRelativePath string,
) (bool, error) {
	same, sameErr := s.syncTree.SameFile(fromRelativePath, toRelativePath)
	if sameErr != nil {
		return false, fmt.Errorf("comparing shortcut alias projection identity: %w", sameErr)
	}
	if !same {
		return false, nil
	}
	err := s.syncTree.RenameWithTemporarySibling(
		fromRelativePath,
		toRelativePath,
		syncshortcut.ShortcutRootCaseRenameTempInfix,
		syncshortcut.ShortcutRootCaseRenameAttempts,
	)
	switch {
	case errors.Is(err, synctree.ErrSourceNotRemoved):
		// Reached only by a file, which validateShortcutRootProjectionMove refuses; a folder is never linked.
		s.logger.Warn("Moved shortcut alias projection "+toRelativePath+", but could not remove a name it was moved from",
			slog.String("path", toRelativePath), slog.Any("error", err))
	case err != nil:
		return false, fmt.Errorf("moving case-only shortcut alias projection: %w", err)
	}
	return true, nil
}

func (s *shortcutRoots) validateShortcutRootProjectionMove(fromRelativePath string, toRelativePath string) error {
	if err := s.syncTree.ValidateNoSymlinkAncestors(fromRelativePath); err != nil {
		return fmt.Errorf("validating previous shortcut alias projection: %w", err)
	}
	if err := s.syncTree.ValidateNoSymlinkAncestors(filepath.Dir(toRelativePath)); err != nil {
		return fmt.Errorf("validating current shortcut alias parent: %w", err)
	}
	fromState, err := s.syncTree.PathStateNoFollow(fromRelativePath)
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
func (s *shortcutRoots) shortcutRootOnDisk(storedPath string) string {
	return s.syncTree.Spelling(filepath.FromSlash(storedPath))
}

func (s *shortcutRoots) releaseShortcutRootProjectionAfterDrain(ctx context.Context, ack syncshortcut.ShortcutChildDrainAck) error {
	if s == nil || s.store == nil || ack.Ref.IsZero() {
		return nil
	}
	return s.finalizeShortcutRootReleaseByBinding(ctx, ack.Ref)
}

func (s *shortcutRoots) finalizeShortcutRootReleaseByBinding(ctx context.Context, ref syncshortcut.ShortcutChildAckRef) error {
	records, err := s.store.ListShortcutRoots(ctx)
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
		next, recordChanged, recordErr := s.finalizeShortcutRootReleaseRecord(&record)
		changed = changed || recordChanged
		nextRecords = append(nextRecords, next...)
		if recordErr != nil {
			cleanupErr = recordErr
		}
	}
	if changed {
		if err := s.store.ReplaceShortcutRoots(ctx, nextRecords); err != nil {
			return fmt.Errorf("sync: persist shortcut root release: %w", err)
		}
	}
	if cleanupErr != nil {
		return fmt.Errorf("sync: release shortcut root projection: %w", cleanupErr)
	}
	return nil
}

func (s *shortcutRoots) finalizeShortcutRootReleaseRecord(
	record *syncshortcut.ShortcutRootRecord,
) ([]syncshortcut.ShortcutRootRecord, bool, error) {
	if record == nil {
		return nil, false, nil
	}
	normalized := syncshortcut.NormalizeShortcutRootRecord(record)
	if !syncshortcut.ShortcutRootStateAwaitsReleaseCleanup(normalized.State) {
		return []syncshortcut.ShortcutRootRecord{normalized}, false, nil
	}
	result := s.removeShortcutRootProjection(normalized.RelativeLocalPath)
	if result.NotEmpty {
		// The child released its tree before acknowledging, so something
		// arrived since; the child, which owns that content, runs again.
		s.logger.Debug("removed shared folder's local folder is not empty; its final sync runs again",
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
func (s *shortcutRoots) removeShortcutRootProjection(relativeLocalPath string) syncshortcut.ShortcutRootReleaseResult {
	relativePath, ok := syncshortcut.CleanShortcutRootRelativePath(relativeLocalPath)
	if !ok {
		return syncshortcut.ShortcutRootReleaseResult{
			Err: fmt.Errorf("%w: the shared folder's local path leaves the sync folder", synctree.ErrUnsafePath),
		}
	}
	relativePath = s.shortcutRootOnDisk(relativePath)
	if err := s.syncTree.ValidateNoSymlinkAncestors(relativePath); err != nil {
		return syncshortcut.ShortcutRootReleaseResult{Err: fmt.Errorf("checking the removed shared folder's local folder: %w", err)}
	}
	state, err := s.syncTree.PathStateNoFollow(relativePath)
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
	if notEmpty, err := s.shortcutRootHoldsEntries(relativePath); err != nil || notEmpty {
		return syncshortcut.ShortcutRootReleaseResult{NotEmpty: notEmpty, Err: err}
	}
	removeErr := s.syncTree.RemoveEmptyDirNoFollow(relativePath)
	if removeErr == nil {
		return syncshortcut.ShortcutRootReleaseResult{}
	}
	// An entry that arrived after the check refuses the removal; that is the
	// child's again, not a blocked cleanup.
	if notEmpty, err := s.shortcutRootHoldsEntries(relativePath); err == nil && notEmpty {
		return syncshortcut.ShortcutRootReleaseResult{NotEmpty: true}
	}

	return syncshortcut.ShortcutRootReleaseResult{Err: fmt.Errorf("removing the removed shared folder's local folder: %w", removeErr)}
}

// shortcutRootHoldsEntries reports whether the alias root at its on-disk
// spelling holds any entry.
func (s *shortcutRoots) shortcutRootHoldsEntries(onDisk string) (bool, error) {
	empty, err := s.syncTree.DirEmptyNoFollow(onDisk)
	if err != nil {
		return false, fmt.Errorf("reading the removed shared folder's local folder: %w", err)
	}

	return !empty, nil
}
