package sync

import (
	"fmt"
	"log/slog"
	"path"
)

const reconciliationKindNoop = "noop"

// PlanCurrentState builds the current actionable set from SQLite-owned
// comparison and reconciliation rows plus the current durable snapshots.
// Unlike the legacy event-shaped planner, this path treats SQLite as the
// structural diff authority and keeps executable actions as in-memory values.
func (p *Planner) PlanCurrentState(
	comparisons []SQLiteComparisonRow,
	reconciliations []SQLiteReconciliationRow,
	localRows []LocalStateRow,
	remoteRows []RemoteStateRow,
	observationIssues []ObservationIssueRow,
	blockScopes []*BlockScope,
	baseline *Baseline,
	mode Mode,
	config *SafetyConfig,
) (*ActionPlan, error) {
	_ = config

	p.logger.Info("planning current actionable set from sqlite reconciliation",
		slog.Int("comparison_rows", len(comparisons)),
		slog.Int("reconciliation_rows", len(reconciliations)),
		slog.Int("baseline_entries", baseline.Len()),
		slog.Int("observation_issues", len(observationIssues)),
		slog.Int("block_scopes", len(blockScopes)),
		slog.String("mode", mode.String()),
	)

	comparisons, reconciliations, blockFacts := normalizePlannerTruthBlocks(
		comparisons,
		reconciliations,
		observationIssues,
		blockScopes,
	)

	views, comparisonByPath, err := buildSQLitePathViews(comparisons, localRows, remoteRows, baseline, blockFacts)
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

	allActions = expandFolderDeleteCascades(allActions, baseline, views, mode, p.logger)

	deferred := deferredCountsForCurrentActions(allActions, mode)
	admitted := filterCurrentActionsForMode(allActions, mode)
	enrichActionTargets(admitted, baseline)

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
		slog.Int("conflicts", CountConflicts(plan.Actions)),
		slog.Int("synced_updates", counts[ActionUpdateSynced]),
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
	blockFacts map[string]plannerTruthBlockFacts,
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

		view := &PathView{Path: row.Path}
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
		if facts, ok := blockFacts[row.Path]; ok {
			view.ObservationIssueType = facts.observationIssueType
			view.ObservationIssueScopeKey = facts.observationIssueScopeKey
			view.LocalReadScope = facts.localReadScope
			view.RemoteReadScope = facts.remoteReadScope
			view.LocalAvailabilityBlocked = facts.localBlocked
			view.RemoteAvailabilityBlocked = facts.remoteBlocked
		}

		views[row.Path] = view
	}

	return views, comparisonByPath, nil
}

type plannerTruthBlockFacts struct {
	observationIssueType     string
	observationIssueScopeKey ScopeKey
	localReadScope           ScopeKey
	remoteReadScope          ScopeKey
	localBlocked             bool
	remoteBlocked            bool
}

func normalizePlannerTruthBlocks(
	comparisons []SQLiteComparisonRow,
	reconciliations []SQLiteReconciliationRow,
	observationIssues []ObservationIssueRow,
	blockScopes []*BlockScope,
) ([]SQLiteComparisonRow, []SQLiteReconciliationRow, map[string]plannerTruthBlockFacts) {
	if len(comparisons) == 0 || len(reconciliations) == 0 {
		return comparisons, reconciliations, nil
	}

	observationByPath := make(map[string]ObservationIssueRow, len(observationIssues))
	for i := range observationIssues {
		if !observationIssueBlocksTruth(observationIssues[i].IssueType) {
			continue
		}
		observationByPath[observationIssues[i].Path] = observationIssues[i]
	}

	comparisonCopy := append([]SQLiteComparisonRow(nil), comparisons...)
	reconciliationCopy := append([]SQLiteReconciliationRow(nil), reconciliations...)
	blockFacts := make(map[string]plannerTruthBlockFacts, len(comparisonCopy))

	for i := range comparisonCopy {
		path := comparisonCopy[i].Path
		observationIssue, hasObservationIssue := observationByPath[path]
		localScope := mostSpecificPlannerReadScope(path, blockScopes, func(key ScopeKey) bool {
			return key.IsPermLocalRead()
		})
		remoteScope := mostSpecificPlannerReadScope(path, blockScopes, func(key ScopeKey) bool {
			return key.IsPermRemoteRead()
		})

		localBlocked := localScope.IsPermLocalRead() || (hasObservationIssue && observationIssueBlocksLocalTruth(observationIssue.IssueType))
		remoteBlocked := remoteScope.IsPermRemoteRead() || (hasObservationIssue && observationIssueBlocksRemoteTruth(observationIssue.IssueType))
		if !localBlocked && !remoteBlocked {
			continue
		}

		blockFacts[path] = plannerTruthBlockFacts{
			observationIssueType:     observationIssue.IssueType,
			observationIssueScopeKey: observationIssue.ScopeKey,
			localReadScope:           localScope,
			remoteReadScope:          remoteScope,
			localBlocked:             localBlocked,
			remoteBlocked:            remoteBlocked,
		}

		reconciliationCopy[i].ReconciliationKind = reconciliationKindNoop
	}

	return comparisonCopy, reconciliationCopy, blockFacts
}

func observationIssueBlocksTruth(issueType string) bool {
	return observationIssueBlocksLocalTruth(issueType) || observationIssueBlocksRemoteTruth(issueType)
}

func observationIssueBlocksLocalTruth(issueType string) bool {
	switch issueType {
	case IssueInvalidFilename,
		IssuePathTooLong,
		IssueFileTooLarge,
		IssueCaseCollision,
		IssueHashPanic,
		IssueLocalReadDenied:
		return true
	default:
		return false
	}
}

func observationIssueBlocksRemoteTruth(issueType string) bool {
	return issueType == IssueRemoteReadDenied
}

func mostSpecificPlannerReadScope(
	path string,
	blockScopes []*BlockScope,
	matches func(ScopeKey) bool,
) ScopeKey {
	best := ScopeKey{}
	bestLen := -1

	for i := range blockScopes {
		block := blockScopes[i]
		if block == nil || !matches(block.Key) {
			continue
		}
		scopePath := plannerScopePath(block.Key)
		if !scopePathMatches(path, scopePath) {
			continue
		}
		if len(scopePath) > bestLen {
			best = block.Key
			bestLen = len(scopePath)
		}
	}

	return best
}

func plannerScopePath(key ScopeKey) string {
	switch {
	case key.IsPermDir():
		return key.DirPath()
	case key.IsPermRemote():
		return key.RemotePath()
	default:
		return ""
	}
}

//nolint:gocyclo // Reconciliation kind dispatch is the planner's explicit decision table.
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
	case strUpdateSynced:
		return []Action{MakeAction(ActionUpdateSynced, view)}, nil
	case "conflict_edit_edit":
		return []Action{
			makeConflictCopyAction(view, ConflictEditEdit),
			makeConflictResolvedAction(ActionDownload, view, ConflictEditEdit),
		}, nil
	case "conflict_edit_delete":
		return []Action{
			makeConflictResolvedAction(ActionUpload, view, ConflictEditDelete),
		}, nil
	case "conflict_create_create":
		return []Action{
			makeConflictCopyAction(view, ConflictCreateCreate),
			makeConflictResolvedAction(ActionDownload, view, ConflictCreateCreate),
		}, nil
	case strLocalMove:
		if cmp.ComparisonKind != "local_move_source" {
			return nil, nil
		}
		if rec.LocalMoveTarget == "" {
			return nil, fmt.Errorf("sync: local_move source %q missing target path", rec.Path)
		}
		action := MakeAction(ActionRemoteMove, view)
		action.OldPath = rec.Path
		action.Path = rec.LocalMoveTarget
		return []Action{action}, nil
	case strRemoteMove:
		if cmp.ComparisonKind != "remote_move_dest" {
			return nil, nil
		}
		if rec.RemoteMoveSource == "" {
			return nil, fmt.Errorf("sync: remote_move destination %q missing source path", rec.Path)
		}
		action := MakeAction(ActionLocalMove, view)
		action.OldPath = rec.RemoteMoveSource
		return []Action{action}, nil
	default:
		return nil, fmt.Errorf("sync: unsupported reconciliation kind %q for %s", rec.ReconciliationKind, rec.Path)
	}
}

func deferredCountsForCurrentActions(actions []Action, mode Mode) DeferredCounts {
	if mode == SyncBidirectional || len(actions) == 0 {
		return DeferredCounts{}
	}

	var deferred DeferredCounts
	for i := range actions {
		if !actionAllowedInMode(&actions[i], mode) {
			deferred.AddAction(&actions[i])
		}
	}

	return deferred
}

func filterCurrentActionsForMode(actions []Action, mode Mode) []Action {
	if mode == SyncBidirectional || len(actions) == 0 {
		return actions
	}

	filtered := make([]Action, 0, len(actions))
	for i := range actions {
		if actionAllowedInMode(&actions[i], mode) {
			filtered = append(filtered, actions[i])
		}
	}

	return filtered
}

func localStateFromSnapshotRow(row *LocalStateRow) *LocalState {
	if row == nil {
		return nil
	}

	return &LocalState{
		Name:     path.Base(row.Path),
		ItemType: row.ItemType,
		Size:     row.Size,
		Hash:     row.Hash,
		Mtime:    row.Mtime,
	}
}

func remoteStateFromSnapshotRow(row *RemoteStateRow) *RemoteState {
	if row == nil {
		return nil
	}

	return &RemoteState{
		ItemID:    row.ItemID,
		DriveID:   row.DriveID,
		ParentID:  row.ParentID,
		Name:      path.Base(row.Path),
		ItemType:  row.ItemType,
		Size:      row.Size,
		Hash:      row.Hash,
		Mtime:     row.Mtime,
		ETag:      row.ETag,
		IsDeleted: false,
	}
}
