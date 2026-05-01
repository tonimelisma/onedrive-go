package sync

import (
	"fmt"
	"log/slog"
	"path"
	"strings"
)

// PlanCurrentState builds the current actionable set from SQLite-owned
// comparison and reconciliation rows plus the current durable snapshots. SQLite
// owns structural diff authority; Go maps those rows to runtime actions and
// applies action-level normalization for execution semantics.
func (p *Planner) PlanCurrentState(
	comparisons []SQLiteComparisonRow,
	reconciliations []SQLiteReconciliationRow,
	localRows []LocalStateRow,
	remoteRows []RemoteStateRow,
	observationIssues []ObservationIssueRow,
	baseline *Baseline,
	mount plannerMountContext,
	mode SyncMode,
) (*ActionPlan, error) {
	p.logger.Info("planning current actionable set from sqlite reconciliation",
		slog.Int("comparison_rows", len(comparisons)),
		slog.Int("reconciliation_rows", len(reconciliations)),
		slog.Int("baseline_entries", baseline.Len()),
		slog.Int("observation_issues", len(observationIssues)),
		slog.String("mode", mode.String()),
	)

	truthPaths := comparisonPaths(comparisons)
	truthIndex := NewTruthAvailabilityIndex(observationIssues)
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

	plan := &ActionPlan{
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
	return preserveParentFoldersForRunnableDescendants(actions, mode)
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

func comparisonPaths(rows []SQLiteComparisonRow) []string {
	if len(rows) == 0 {
		return nil
	}

	paths := make([]string, 0, len(rows))
	for i := range rows {
		paths = append(paths, rows[i].Path)
	}

	return paths
}

func logActionPlanSummary(logger *slog.Logger, message string, plan *ActionPlan) {
	if logger == nil || plan == nil {
		return
	}

	counts := CountByType(plan.Actions)
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
	comparisons []SQLiteComparisonRow,
	localRows []LocalStateRow,
	remoteRows []RemoteStateRow,
	baseline *Baseline,
	truthStatusByPath map[string]PathTruthStatus,
) (map[string]*PathView, map[string]*SQLiteComparisonRow, error) {
	localByPath := make(map[string]LocalStateRow, len(localRows))
	for i := range localRows {
		localByPath[localRows[i].Path] = localRows[i]
	}

	remoteByPath := make(map[string]RemoteStateRow, len(remoteRows))
	for i := range remoteRows {
		remoteByPath[remoteRows[i].Path] = remoteRows[i]
	}

	views := make(map[string]*PathView, len(comparisons))
	comparisonByPath := make(map[string]*SQLiteComparisonRow, len(comparisons))

	for i := range comparisons {
		row := comparisons[i]
		comparisonByPath[row.Path] = &comparisons[i]

		truthStatus, ok := truthStatusByPath[row.Path]
		if !ok {
			return nil, nil, fmt.Errorf("sync: missing truth status for comparison path %q", row.Path)
		}

		view := &PathView{
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
	rec *SQLiteReconciliationRow,
	cmp *SQLiteComparisonRow,
	views map[string]*PathView,
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
		return []Action{MakeAction(ActionCleanup, view)}, nil
	case "folder_create_local":
		return []Action{makeFolderCreate(view, CreateLocal)}, nil
	case "folder_create_remote":
		return []Action{makeFolderCreate(view, CreateRemote)}, nil
	case "upload":
		return []Action{MakeAction(ActionUpload, view)}, nil
	case "download":
		return []Action{MakeAction(ActionDownload, view)}, nil
	case strLocalDelete:
		return []Action{MakeAction(ActionLocalDelete, view)}, nil
	case strRemoteDelete:
		return []Action{MakeAction(ActionRemoteDelete, view)}, nil
	case strBaselineUpdate:
		return []Action{MakeAction(ActionBaselineUpdate, view)}, nil
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
	case strLocalMove:
		return buildLocalMoveReconciliationActions(rec, cmp, view, views)
	case strRemoteMove:
		return buildRemoteMoveReconciliationActions(rec, cmp, view)
	default:
		return nil, fmt.Errorf("sync: unsupported reconciliation kind %q for %s", rec.ReconciliationKind, rec.Path)
	}
}

func buildLocalMoveReconciliationActions(
	rec *SQLiteReconciliationRow,
	cmp *SQLiteComparisonRow,
	view *PathView,
	views map[string]*PathView,
) ([]Action, error) {
	if cmp.ComparisonKind != "local_move_source" {
		return nil, nil
	}
	if rec.LocalMoveTarget == "" {
		return nil, fmt.Errorf("sync: local_move source %q missing target path", rec.Path)
	}

	action := MakeAction(ActionRemoteMove, view)
	action.OldPath = rec.Path
	action.Path = rec.LocalMoveTarget

	actions := []Action{action}
	if upload := localMoveContentUpdateAfterMove(rec.LocalMoveTarget, view, views); upload != nil {
		actions = append(actions, *upload)
	}

	return actions, nil
}

func localMoveContentUpdateAfterMove(
	targetPath string,
	sourceView *PathView,
	views map[string]*PathView,
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
	action := MakeAction(ActionUpload, &updateView)
	action.Path = targetPath

	return &action
}

func localFileDiffersFromBaseline(local *LocalState, baseline *BaselineEntry) bool {
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

func buildRemoteMoveReconciliationActions(
	rec *SQLiteReconciliationRow,
	cmp *SQLiteComparisonRow,
	view *PathView,
) ([]Action, error) {
	if cmp.ComparisonKind != "remote_move_dest" {
		return nil, nil
	}
	if rec.RemoteMoveSource == "" {
		return nil, fmt.Errorf("sync: remote_move destination %q missing source path", rec.Path)
	}

	action := MakeAction(ActionLocalMove, view)
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

func localStateFromSnapshotRow(row *LocalStateRow) *LocalState {
	if row == nil {
		return nil
	}

	return &LocalState{
		Name:             path.Base(row.Path),
		ItemType:         row.ItemType,
		Size:             row.Size,
		Hash:             row.Hash,
		Mtime:            row.Mtime,
		LocalDevice:      row.LocalDevice,
		LocalInode:       row.LocalInode,
		LocalHasIdentity: row.LocalHasIdentity,
	}
}

func remoteStateFromSnapshotRow(row *RemoteStateRow) *RemoteState {
	if row == nil {
		return nil
	}

	return &RemoteState{
		ItemID:    row.ItemID,
		DriveID:   row.DriveID,
		Name:      path.Base(row.Path),
		ItemType:  row.ItemType,
		Size:      row.Size,
		Hash:      row.Hash,
		Mtime:     row.Mtime,
		ETag:      row.ETag,
		IsDeleted: false,
	}
}
