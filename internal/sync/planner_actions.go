package sync

import (
	"errors"
	"fmt"
	"log/slog"
	"path"
	"sort"
	"strings"
)

// errFolderTransferPlanned reports a reconciliation row that maps a folder to
// a content transfer. Folders are created, moved or deleted, never uploaded
// or downloaded (D-057); a row that says otherwise is a comparison bug.
var errFolderTransferPlanned = errors.New("sync: folder planned as a content transfer")

// PlanCurrentState builds the current actionable set from SQLite-owned
// comparison and reconciliation rows plus the current durable snapshots. SQLite
// owns structural diff authority; Go maps those rows to runtime actions and
// applies action-level normalization for execution semantics.
func (p *planner) PlanCurrentState(
	comparisons []sQLiteComparisonRow,
	reconciliations []sQLiteReconciliationRow,
	localRows []localStateRow,
	remoteRows []remoteStateRow,
	observationIssues []ObservationIssueRow,
	baseline *Baseline,
	mount plannerMountContext,
	mode SyncMode,
) (*actionPlan, error) {
	p.logger.Debug("planning current actionable set from sqlite reconciliation",
		slog.Int("comparison_rows", len(comparisons)),
		slog.Int("reconciliation_rows", len(reconciliations)),
		slog.Int("baseline_entries", baseline.Len()),
		slog.Int("observation_issues", len(observationIssues)),
		slog.String("mode", mode.String()),
	)

	truthPaths := comparisonPaths(comparisons)
	truthIndex := newTruthAvailabilityIndex(observationIssues)
	truthStatusByPath := truthIndex.StatusByPath(truthPaths)

	views, comparisonByPath, err := buildSQLitePathViews(comparisons, localRows, remoteRows, baseline, truthStatusByPath)
	if err != nil {
		return nil, err
	}

	allActions := make([]Action, 0, len(reconciliations))
	for i := range reconciliations {
		rec := reconciliations[i]
		cmp, ok := comparisonByPath[rec.Path]
		if !ok {
			return nil, fmt.Errorf("sync: missing comparison row for reconciliation path %q", rec.Path)
		}

		actions, err := buildActionsForReconciliation(&rec, cmp, views)
		if errors.Is(err, errFolderTransferPlanned) {
			// A comparison bug, not a user state: transferring a directory
			// as file content can only fail and be replanned forever. The
			// rest of the plan stays runnable.
			p.logger.Error("planner dropped an impossible folder transfer",
				slog.String("path", rec.Path),
				slog.String("reconciliation", rec.ReconciliationKind),
				slog.String("error", err.Error()),
			)
			continue
		}
		if err != nil {
			return nil, err
		}

		allActions = append(allActions, actions...)
	}

	normalizedActions := normalizeCurrentPlanActions(allActions, mode)
	admitted, deferred := partitionCurrentActionsForMode(normalizedActions, mode)
	bindMountContext(admitted, mount)

	deps := buildDependencies(admitted)
	if err := detectDependencyCycle(deps); err != nil {
		return nil, err
	}

	plan := &actionPlan{
		Actions:        admitted,
		Deps:           deps,
		DeferredByMode: deferred,
	}

	logActionPlanSummary(p.logger, "sqlite actionable-set plan complete", plan)

	return plan, nil
}

// normalizeCurrentPlanActions keeps SQL as structural authority, then applies
// action-only execution semantics that cannot be represented by a single path's
// reconciliation row.
func normalizeCurrentPlanActions(actions []Action, mode SyncMode) []Action {
	actions = omitDescendantRemoteMoveActionsCoveredByGraphFolderMoves(actions)
	actions = rebaseLocalWritesThroughLocalFolderMoves(actions)
	actions = omitDescendantLocalMovesCoveredByLocalFolderMoves(actions)
	return preserveParentFoldersForRunnableDescendants(actions, mode)
}

// rebaseLocalWritesThroughLocalFolderMoves composes the two sides' moves. A
// remote-origin action that writes locally carries remote paths, but a folder
// the user renamed locally in the same plan no longer exists at its old local
// path: OneDrive moving photos into docs/x while docs was renamed locally to
// renamed-docs must land at renamed-docs/x. Applied at the old path, the move
// re-created docs/ locally, and the next pass uploaded the stray folders.
// Remote-side writes need no rebasing: they address parents by item ID.
func rebaseLocalWritesThroughLocalFolderMoves(actions []Action) []Action {
	var folderMoves []graphFolderMoveBoundary
	for i := range actions {
		action := &actions[i]
		if action.Type != ActionRemoteMove || resolveItemType(action.View) != ItemTypeFolder || action.OldPath == "" {
			continue
		}
		folderMoves = append(folderMoves, graphFolderMoveBoundary{
			oldPrefix: action.OldPath + "/",
			newPrefix: action.Path + "/",
		})
	}
	if len(folderMoves) == 0 {
		return actions
	}
	// The deepest moved folder decides, so nested renames compose.
	sort.Slice(folderMoves, func(i, j int) bool {
		return len(folderMoves[i].oldPrefix) > len(folderMoves[j].oldPrefix)
	})

	rebased := make([]Action, len(actions))
	copy(rebased, actions)
	for i := range rebased {
		if !writesLocally(&rebased[i]) {
			continue
		}
		rebased[i].Path = rebaseThroughFolderMoves(rebased[i].Path, folderMoves)
		rebased[i].OldPath = rebaseThroughFolderMoves(rebased[i].OldPath, folderMoves)
	}

	return rebased
}

// omitDescendantLocalMovesCoveredByLocalFolderMoves is the local mirror of
// omitDescendantRemoteMoveActionsCoveredByGraphFolderMoves. A folder moved on
// OneDrive arrives with its descendants' new remote paths, so each child also
// pairs as a remote move; renaming the folder locally already carries every
// child with an unchanged suffix. Left in, a child move could run first,
// create the folder's destination, and make the folder move fail on a taken
// path forever.
func omitDescendantLocalMovesCoveredByLocalFolderMoves(actions []Action) []Action {
	var folderMoves []graphFolderMoveBoundary
	for i := range actions {
		action := &actions[i]
		if action.Type != ActionLocalMove || resolveItemType(action.View) != ItemTypeFolder || action.OldPath == "" {
			continue
		}
		folderMoves = append(folderMoves, graphFolderMoveBoundary{
			oldPrefix: action.OldPath + "/",
			newPrefix: action.Path + "/",
		})
	}
	if len(folderMoves) == 0 {
		return actions
	}

	kept := make([]Action, 0, len(actions))
	for i := range actions {
		action := actions[i]
		if action.Type == ActionLocalMove && moveCoveredByFolderMove(action.OldPath, action.Path, folderMoves) {
			continue
		}
		kept = append(kept, action)
	}

	return kept
}

func moveCoveredByFolderMove(oldPath, newPath string, folderMoves []graphFolderMoveBoundary) bool {
	if oldPath == "" {
		return false
	}
	for _, move := range folderMoves {
		if !strings.HasPrefix(oldPath, move.oldPrefix) || !strings.HasPrefix(newPath, move.newPrefix) {
			continue
		}
		if strings.TrimPrefix(oldPath, move.oldPrefix) == strings.TrimPrefix(newPath, move.newPrefix) {
			return true
		}
	}

	return false
}

// writesLocally reports remote-origin actions that apply OneDrive's state to
// the local tree, addressed by path.
func writesLocally(action *Action) bool {
	switch action.Type {
	case ActionLocalMove, ActionDownload, ActionLocalDelete, ActionConflictCopy:
		return true
	case ActionFolderCreate:
		return action.CreateSide == createLocal
	case ActionUpload, ActionRemoteMove, ActionRemoteDelete, ActionBaselineUpdate, ActionCleanup:
		return false
	default:
		return false
	}
}

func rebaseThroughFolderMoves(p string, folderMoves []graphFolderMoveBoundary) string {
	if p == "" {
		return p
	}
	for _, move := range folderMoves {
		if strings.HasPrefix(p, move.oldPrefix) {
			return move.newPrefix + strings.TrimPrefix(p, move.oldPrefix)
		}
	}

	return p
}

type graphFolderMoveBoundary struct {
	oldPrefix string
	newPrefix string
}

func omitDescendantRemoteMoveActionsCoveredByGraphFolderMoves(actions []Action) []Action {
	var folderMoves []graphFolderMoveBoundary
	for i := range actions {
		action := actions[i]
		if action.Type != ActionRemoteMove || resolveItemType(action.View) != ItemTypeFolder || action.OldPath == "" {
			continue
		}
		folderMoves = append(folderMoves, graphFolderMoveBoundary{
			oldPrefix: action.OldPath + "/",
			newPrefix: action.Path + "/",
		})
	}
	if len(folderMoves) == 0 {
		return append([]Action(nil), actions...)
	}

	normalized := make([]Action, 0, len(actions))
	for i := range actions {
		action := actions[i]
		if descendantRemoteMoveActionCoveredByGraphFolderMove(action, folderMoves) {
			continue
		}
		normalized = append(normalized, action)
	}

	return normalized
}

// descendantRemoteMoveActionCoveredByGraphFolderMove recognizes Graph's folder
// move side effect: a folder move sent to Graph already moves unchanged
// descendants with the same suffix under the new folder path.
func descendantRemoteMoveActionCoveredByGraphFolderMove(action Action, folderMoves []graphFolderMoveBoundary) bool {
	if action.Type != ActionRemoteMove || action.OldPath == "" {
		return false
	}
	for _, move := range folderMoves {
		if !strings.HasPrefix(action.OldPath, move.oldPrefix) || !strings.HasPrefix(action.Path, move.newPrefix) {
			continue
		}
		oldSuffix := strings.TrimPrefix(action.OldPath, move.oldPrefix)
		newSuffix := strings.TrimPrefix(action.Path, move.newPrefix)
		if oldSuffix == newSuffix {
			return true
		}
	}

	return false
}

func comparisonPaths(rows []sQLiteComparisonRow) []string {
	if len(rows) == 0 {
		return nil
	}

	paths := make([]string, 0, len(rows))
	for i := range rows {
		paths = append(paths, rows[i].Path)
	}

	return paths
}

func logActionPlanSummary(logger *slog.Logger, message string, plan *actionPlan) {
	if logger == nil || plan == nil {
		return
	}

	counts := countByType(plan.Actions)
	logger.Info(message,
		slog.Int("total_actions", len(plan.Actions)),
		slog.Int("folder_creates", counts[ActionFolderCreate]),
		slog.Int("moves", counts[ActionLocalMove]+counts[ActionRemoteMove]),
		slog.Int("downloads", counts[ActionDownload]),
		slog.Int("uploads", counts[ActionUpload]),
		slog.Int("local_deletes", counts[ActionLocalDelete]),
		slog.Int("remote_deletes", counts[ActionRemoteDelete]),
		slog.Int("conflict_copies", counts[ActionConflictCopy]),
		slog.Int("baseline_updates", counts[ActionBaselineUpdate]),
		slog.Int("cleanups", counts[ActionCleanup]),
		slog.Int("deferred_folder_creates", plan.DeferredByMode.FolderCreates),
		slog.Int("deferred_moves", plan.DeferredByMode.Moves),
		slog.Int("deferred_downloads", plan.DeferredByMode.Downloads),
		slog.Int("deferred_uploads", plan.DeferredByMode.Uploads),
		slog.Int("deferred_local_deletes", plan.DeferredByMode.LocalDeletes),
		slog.Int("deferred_remote_deletes", plan.DeferredByMode.RemoteDeletes),
	)
}

func buildSQLitePathViews(
	comparisons []sQLiteComparisonRow,
	localRows []localStateRow,
	remoteRows []remoteStateRow,
	baseline *Baseline,
	truthStatusByPath map[string]pathTruthStatus,
) (map[string]*pathView, map[string]*sQLiteComparisonRow, error) {
	localByPath := make(map[string]localStateRow, len(localRows))
	for i := range localRows {
		localByPath[localRows[i].Path] = localRows[i]
	}

	remoteByPath := make(map[string]remoteStateRow, len(remoteRows))
	for i := range remoteRows {
		remoteByPath[remoteRows[i].Path] = remoteRows[i]
	}

	views := make(map[string]*pathView, len(comparisons))
	comparisonByPath := make(map[string]*sQLiteComparisonRow, len(comparisons))

	for i := range comparisons {
		row := comparisons[i]
		comparisonByPath[row.Path] = &comparisons[i]

		truthStatus, ok := truthStatusByPath[row.Path]
		if !ok {
			return nil, nil, fmt.Errorf("sync: missing truth status for comparison path %q", row.Path)
		}

		view := &pathView{
			Path:        row.Path,
			TruthStatus: truthStatus,
		}
		if row.BaselinePresent {
			entry, ok := baseline.GetByPath(row.Path)
			if !ok {
				return nil, nil, fmt.Errorf("sync: missing baseline entry for comparison path %q", row.Path)
			}
			view.Baseline = entry
		}
		if row.LocalPresent {
			localRow, ok := localByPath[row.Path]
			if !ok {
				return nil, nil, fmt.Errorf("sync: missing local_state row for comparison path %q", row.Path)
			}
			view.Local = localStateFromSnapshotRow(&localRow)
		}
		if row.RemotePresent {
			remoteRow, ok := remoteByPath[row.Path]
			if !ok {
				return nil, nil, fmt.Errorf("sync: missing remote_state row for comparison path %q", row.Path)
			}
			view.Remote = remoteStateFromSnapshotRow(&remoteRow)
		}
		views[row.Path] = view
	}

	return views, comparisonByPath, nil
}

//nolint:gocyclo // One switch maps SQL reconciliation kinds to concrete action constructors.
func buildActionsForReconciliation(
	rec *sQLiteReconciliationRow,
	cmp *sQLiteComparisonRow,
	views map[string]*pathView,
) ([]Action, error) {
	if rec == nil || cmp == nil {
		return nil, fmt.Errorf("sync: reconciliation row requires comparison context")
	}

	view := views[rec.Path]
	if view == nil {
		return nil, fmt.Errorf("sync: missing path view for reconciliation path %q", rec.Path)
	}
	if plannerSuppressesUnavailableTruth(&view.TruthStatus) {
		return nil, nil
	}

	switch rec.ReconciliationKind {
	case "noop":
		return nil, nil
	case "baseline_remove":
		return []Action{makeAction(ActionCleanup, view)}, nil
	case "folder_create_local":
		return []Action{makeFolderCreate(view, createLocal)}, nil
	case "folder_create_remote":
		return []Action{makeFolderCreate(view, CreateRemote)}, nil
	case "upload":
		if view.Local != nil && view.Local.ItemType == ItemTypeFolder {
			return nil, fmt.Errorf("%w: upload of local folder %s", errFolderTransferPlanned, rec.Path)
		}
		return []Action{makeAction(ActionUpload, view)}, nil
	case "download":
		if view.Remote != nil && view.Remote.ItemType == ItemTypeFolder {
			return nil, fmt.Errorf("%w: download of remote folder %s", errFolderTransferPlanned, rec.Path)
		}
		return []Action{makeAction(ActionDownload, view)}, nil
	case strLocalDelete:
		return []Action{makeAction(ActionLocalDelete, view)}, nil
	case strRemoteDelete:
		return []Action{makeAction(ActionRemoteDelete, view)}, nil
	case strBaselineUpdate:
		return []Action{makeAction(ActionBaselineUpdate, view)}, nil
	case "conflict_edit_edit":
		return []Action{
			makeConflictCopyAction(view),
			makeDownloadAfterConflictCopyAction(view),
		}, nil
	case "conflict_edit_delete":
		return []Action{
			makeCreateUploadAction(view),
		}, nil
	case "conflict_create_create":
		return []Action{
			makeConflictCopyAction(view),
			makeDownloadAfterConflictCopyAction(view),
		}, nil
	case "replace_remote":
		return buildReplaceRemoteActions(view), nil
	case "replace_local":
		return buildReplaceLocalActions(view), nil
	case strLocalMove:
		return buildLocalMoveReconciliationActions(rec, cmp, view, views)
	case strRemoteMove:
		return buildRemoteMoveReconciliationActions(rec, cmp, view, views)
	default:
		return nil, fmt.Errorf("sync: unsupported reconciliation kind %q for %s", rec.ReconciliationKind, rec.Path)
	}
}

// buildReplaceRemoteActions replaces the OneDrive item at a path whose local
// item is now of another type (or whose remote type changed while local
// content also changed): delete the old remote item, then create the local
// item as new content. buildDependencies orders the create after the delete.
//
// The create's view has no baseline or remote: it is a brand-new item, and by
// the time it dispatches the delete outcome has removed the old remote_state
// row, so remote freshness expects absence.
func buildReplaceRemoteActions(view *pathView) []Action {
	deleteAction := makeAction(ActionRemoteDelete, view)

	createView := &pathView{
		Path:        view.Path,
		Local:       view.Local,
		TruthStatus: view.TruthStatus,
	}
	if view.Local != nil && view.Local.ItemType == ItemTypeFolder {
		return []Action{deleteAction, makeFolderCreate(createView, CreateRemote)}
	}

	return []Action{deleteAction, makeCreateUploadAction(createView)}
}

// buildReplaceLocalActions replaces the local item at a path whose OneDrive
// item is now of another type while local truth is unchanged: delete the old
// local item, then materialize the remote one.
//
// The delete's view describes the old local item (resolveItemType prefers the
// remote side, which already has the new type). local_state is refreshed only
// by observation, so the create keeps the planned local row for freshness, and
// a download requires its target to be missing, as after a conflict copy.
func buildReplaceLocalActions(view *pathView) []Action {
	deleteView := &pathView{
		Path:        view.Path,
		Local:       view.Local,
		Remote:      view.Remote,
		Baseline:    view.Baseline,
		TruthStatus: view.TruthStatus,
	}
	deleteAction := makeAction(ActionLocalDelete, deleteView)

	createView := &pathView{
		Path:        view.Path,
		Local:       view.Local,
		Remote:      view.Remote,
		TruthStatus: view.TruthStatus,
	}
	if view.Remote != nil && view.Remote.ItemType == ItemTypeFolder {
		return []Action{deleteAction, makeFolderCreate(createView, createLocal)}
	}

	download := makeAction(ActionDownload, createView)
	download.RequireMissingLocalTarget = true

	return []Action{deleteAction, download}
}

func buildLocalMoveReconciliationActions(
	rec *sQLiteReconciliationRow,
	cmp *sQLiteComparisonRow,
	view *pathView,
	views map[string]*pathView,
) ([]Action, error) {
	if cmp.ComparisonKind != "local_move_source" {
		return nil, nil
	}
	if rec.LocalMoveTarget == "" {
		return nil, fmt.Errorf("sync: local_move source %q missing target path", rec.Path)
	}

	action := makeAction(ActionRemoteMove, view)
	action.OldPath = rec.Path
	action.Path = rec.LocalMoveTarget
	if targetView := views[rec.LocalMoveTarget]; targetView != nil {
		action.MovedLocal = targetView.Local
	}

	actions := []Action{action}
	if upload := localMoveContentUpdateAfterMove(rec.LocalMoveTarget, view, views); upload != nil {
		actions = append(actions, *upload)
	}

	return actions, nil
}

func localMoveContentUpdateAfterMove(
	targetPath string,
	sourceView *pathView,
	views map[string]*pathView,
) *Action {
	if sourceView == nil || sourceView.Baseline == nil || sourceView.Baseline.ItemType != ItemTypeFile {
		return nil
	}
	targetView := views[targetPath]
	if targetView == nil || targetView.Local == nil {
		return nil
	}
	if !localFileDiffersFromBaseline(targetView.Local, sourceView.Baseline) {
		return nil
	}

	updateView := *targetView
	updateView.Baseline = sourceView.Baseline
	updateView.Remote = sourceView.Remote
	action := makeAction(ActionUpload, &updateView)
	action.Path = targetPath

	return &action
}

func localFileDiffersFromBaseline(local *localState, baseline *BaselineEntry) bool {
	if local == nil || baseline == nil {
		return false
	}
	if baseline.ItemType != ItemTypeFile || local.ItemType != ItemTypeFile {
		return false
	}
	if baseline.LocalHash != "" || local.Hash != "" {
		return baseline.LocalHash != local.Hash
	}
	if baseline.LocalSizeKnown && baseline.LocalSize != local.Size {
		return true
	}

	return baseline.LocalMtime != local.Mtime
}

// buildRemoteMoveReconciliationActions turns a remote move into a local
// rename. The destination path has no baseline row of its own, so the action
// view carries the source's baseline: executor preconditions and freshness
// validate the local source against it, and the move outcome republishes its
// local half at the destination. Without it the moved row would lose its
// local hash and identity, and the next scan would re-upload unchanged content.
func buildRemoteMoveReconciliationActions(
	rec *sQLiteReconciliationRow,
	cmp *sQLiteComparisonRow,
	view *pathView,
	views map[string]*pathView,
) ([]Action, error) {
	if cmp.ComparisonKind != "remote_move_dest" {
		return nil, nil
	}
	if rec.RemoteMoveSource == "" {
		return nil, fmt.Errorf("sync: remote_move destination %q missing source path", rec.Path)
	}

	moveView := view
	if sourceView := views[rec.RemoteMoveSource]; sourceView != nil && sourceView.Baseline != nil && view.Baseline == nil {
		withSource := *view
		withSource.Baseline = sourceView.Baseline
		moveView = &withSource
	}

	action := makeAction(ActionLocalMove, moveView)
	action.OldPath = rec.RemoteMoveSource

	return []Action{action}, nil
}

func partitionCurrentActionsForMode(actions []Action, mode SyncMode) ([]Action, DeferredCounts) {
	admitted := make([]Action, 0, len(actions))
	var deferred DeferredCounts
	for i := range actions {
		if actionAllowedInMode(&actions[i], mode) {
			admitted = append(admitted, actions[i])
		} else {
			deferred.AddAction(&actions[i])
		}
	}

	return admitted, deferred
}

func localStateFromSnapshotRow(row *localStateRow) *localState {
	if row == nil {
		return nil
	}

	return &localState{
		Name:             path.Base(row.Path),
		ItemType:         row.ItemType,
		Size:             row.Size,
		Hash:             row.Hash,
		Mtime:            row.Mtime,
		LocalCtime:       row.Ctime,
		LocalDevice:      row.LocalDevice,
		LocalInode:       row.LocalInode,
		LocalHasIdentity: row.LocalHasIdentity,
	}
}

func remoteStateFromSnapshotRow(row *remoteStateRow) *remoteState {
	if row == nil {
		return nil
	}

	return &remoteState{
		ItemID:    row.ItemID,
		DriveID:   row.DriveID,
		Name:      path.Base(row.Path),
		ItemType:  row.ItemType,
		Size:      row.Size,
		SizeKnown: row.SizeKnown,
		Hash:      row.Hash,
		Mtime:     row.Mtime,
		ETag:      row.ETag,
		IsDeleted: false,
	}
}
