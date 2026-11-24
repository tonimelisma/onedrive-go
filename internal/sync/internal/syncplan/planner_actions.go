package syncplan

import (
	"errors"
	"fmt"
	"log/slog"
	"path"
	"slices"
	"sort"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// errFolderTransferPlanned reports a reconciliation row that maps a folder to
// a content transfer. Folders are created, moved or deleted, never uploaded
// or downloaded (D-057); a row that says otherwise is a comparison bug, and
// it fails the plan like a row outside the decision table (R-6.2.16):
// dropped, the path would silently stop syncing.
var errFolderTransferPlanned = errors.New("sync: folder planned as a content transfer")

// PlanCurrentState builds the current actionable set from SQLite-owned
// comparison and reconciliation rows plus the current durable snapshots. SQLite
// owns structural diff authority; Go maps those rows to runtime actions and
// applies action-level normalization for execution semantics. A row the
// decision table did not classify fails the whole plan before any row is
// planned (validatePlannerKinds), and a built action outside the action enums
// fails it before any rule reads one (planBuiltActions).
func (p *Planner) PlanCurrentState(
	comparisons []DecidedComparison,
	reconciliations []syncdomain.SQLiteReconciliationRow,
	localRows []syncdomain.LocalStateRow,
	remoteRows []syncdomain.RemoteStateRow,
	observationIssues []syncdomain.ObservationIssueRow,
	baseline *syncdomain.BaselineSnapshot,
	mount PlannerMountContext,
	mode syncdomain.SyncMode,
) (*syncdomain.ActionPlan, error) {
	p.logger.Debug("planning current actionable set from sqlite reconciliation",
		slog.Int("comparison_rows", len(comparisons)),
		slog.Int("reconciliation_rows", len(reconciliations)),
		slog.Int("baseline_entries", baseline.Len()),
		slog.Int("observation_issues", len(observationIssues)),
		slog.String("mode", mode.String()),
	)

	if err := validatePlannerKinds(comparisons, reconciliations); err != nil {
		return nil, err
	}

	truthPaths := comparisonPaths(comparisons)
	truthIndex := syncdomain.NewTruthAvailabilityIndex(observationIssues)
	truthStatusByPath := truthIndex.StatusByPath(truthPaths)

	views, comparisonByPath, err := buildSQLitePathViews(comparisons, localRows, remoteRows, baseline, truthStatusByPath)
	if err != nil {
		return nil, err
	}

	var reconciled []syncdomain.Action
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

		reconciled = append(reconciled, actions...)
	}
	reconciled = append(reconciled, createFoldersOneDriveMadeAtAMovedSource(comparisons, views)...)
	inputs := moveCompositionInputs(comparisons, views, remoteRows, localRows)
	reconciled = giveWayFoldersReplacedOnOneDrive(&inputs, reconciled, comparisons, views, localRows)
	displaceFoldersDeletedHere(&inputs, localDeletePaths(reconciled))
	composition := composeMoves(inputs.nodes, inputs.occupancy, inputs.frozen)
	logMoveNotices(p.logger, composition.notices)
	composed, waiting := buildComposedMoveActions(&composition, &inputs, views, baseline)

	return p.planBuiltActions(reconciled, composed, waiting, &composition, &inputs, mount, mode)
}

// createFoldersOneDriveMadeAtAMovedSource plans here the folder OneDrive put
// at the path a synced item left by this computer's move, while the path is
// empty here (from the held start, OneDrive moved f into its new X2 and X2
// onto f's path, while f had moved here into the new x). SQLite reconciles
// the path as the source of the local move alone, which plans nothing there,
// so the folder was never made here, and the move of f into it, OneDrive's
// place winning, failed for want of its parent on every pass. The create
// records OneDrive's folder at the path; a move of the item that left it
// commits by item ID wherever it lands.
func createFoldersOneDriveMadeAtAMovedSource(
	comparisons []DecidedComparison, views map[string]*syncdomain.PathView,
) []syncdomain.Action {
	var creates []syncdomain.Action
	for i := range comparisons {
		cmp := &comparisons[i]
		view := views[cmp.Path]
		if cmp.ComparisonKind != syncdomain.ComparisonLocalMoveSource || cmp.LocalType.Present() ||
			cmp.RemoteType != syncdomain.SideFolder || !cmp.RemoteItemReplaced || view == nil || view.Remote == nil ||
			plannerSuppressesUnavailableTruth(&view.TruthStatus) {
			continue
		}
		creates = append(creates, makeFolderCreate(view, syncdomain.CreateLocal))
	}

	return creates
}

// localDeletePaths lists the paths the reconciliation actions delete here.
func localDeletePaths(reconciled []syncdomain.Action) map[string]bool {
	paths := map[string]bool{}
	for i := range reconciled {
		if reconciled[i].Type == syncdomain.ActionLocalDelete {
			paths[reconciled[i].Path] = true
		}
	}

	return paths
}

// planBuiltActions turns the actions PlanCurrentState built, from the
// reconciliation rows (reconciled) and by move composition (composed), into
// the plan: it drops the reconciliation work composition replaces, normalizes,
// holds back the deletes that work held for a later pass still needs
// (withholdDeletesOfHeldWork), admits by mode (and, for a mount that
// releases its local tree, by leaveOutNewLocalItems) and orders, without the
// actions caught in a dependency cycle or the deletes they hold
// (admitWithoutCycles). waiting is the creates that wait with a move held for
// a later pass, built aside (recreatedSourceCreates). Every one of
// those steps decides by action type, so the built actions are checked against
// the enums first (validatePlannedActions): an action outside them fails the
// plan, as a row outside the decision table does, before any step can answer
// it.
func (p *Planner) planBuiltActions(
	reconciled, composed, waiting []syncdomain.Action,
	composition *moveComposition,
	inputs *moveInputs,
	mount PlannerMountContext,
	mode syncdomain.SyncMode,
) (*syncdomain.ActionPlan, error) {
	for _, built := range [][]syncdomain.Action{reconciled, composed} {
		if err := validatePlannedActions(built); err != nil {
			return nil, err
		}
	}

	allActions := dropReconciliationOfComposedMoves(reconciled, composition, inputs.identified, mode)
	allActions = append(allActions, composed...)

	normalizedActions, trimmedActions, trimmedInMode := normalizeCurrentPlanActions(
		allActions, remoteToLocalFolderPrefixes(inputs.nodes), mode)
	trimmed := len(trimmedActions)
	normalizedActions, droppedCreateActions := dropCreatesOntoComposedItems(p.logger, normalizedActions, inputs, composition)
	droppedCreates, droppedCreatesInMode := len(droppedCreateActions), countActionsAllowedInMode(droppedCreateActions, mode)
	held := slices.Concat(waiting, trimmedActions, droppedCreateActions)
	normalizedActions, withheld, withheldInMode := withholdDeletesOfHeldWork(
		p.logger, normalizedActions, held, inputs, composition, mode)
	normalizedActions = setAsideRemovedContent(p.logger, normalizedActions, inputs, mode)
	if mount.ReleasesLocalTree {
		normalizedActions = p.leaveOutNewLocalItems(normalizedActions, mode)
	}
	admitted, deferred := partitionCurrentActionsForMode(normalizedActions, mode)
	bindMountContext(admitted, mount)

	admitted, deps, inCycles, withheldWithCycles := p.admitWithoutCycles(admitted, held, inputs, composition, mode)
	withheld += withheldWithCycles
	withheldInMode += withheldWithCycles

	plan := &syncdomain.ActionPlan{
		Actions:        admitted,
		Deps:           deps,
		DeferredByMode: deferred,
		FollowUp: syncdomain.PlanFollowUp{
			Renames:    countRenames(admitted),
			Deferred:   len(composition.deferred),
			Dropped:    trimmed + droppedCreates + withheld + inCycles,
			Unfinished: composition.deferredRelocationsInMode(mode) + trimmedInMode + droppedCreatesInMode + withheldInMode + inCycles,
		},
	}

	logActionPlanSummary(p.logger, "sqlite actionable-set plan complete", plan)

	return plan, nil
}

// admitWithoutCycles removes the admitted actions caught in a dependency cycle
// (isolateDependencyCycles), with what waits for them, and, as they are work
// held for the next pass too, the deletes they hold (withholdDeletesOfHeldWork)
// that wait for none of them: the delete of a file whose content a create in
// the cycle writes, when that create itself waits for the delete, so the two
// have no edge (addContentCarrierDeps). It repeats until neither removes more,
// and returns the dependencies of what it keeps and how many actions the
// cycles and the deletes removed; every one of them is admitted, so the mode
// runs it.
//
// A folder delete that closes a cycle through the move of a folder above it
// is held for the next pass first (withholdDeletesInsideFolderMoveCycles), so
// the rest of that cycle runs.
func (p *Planner) admitWithoutCycles(
	admitted, held []syncdomain.Action, inputs *moveInputs, composition *moveComposition, mode syncdomain.SyncMode,
) (kept []syncdomain.Action, deps [][]int, inCycles, withheld int) {
	admitted, heldDeletes := withholdDeletesInsideFolderMoveCycles(p.logger, admitted, mode)
	withheld = len(heldDeletes)
	held = slices.Concat(held, heldDeletes)
	kept, deps, cycled := isolateDependencyCycles(admitted, p.logger)
	inCycles = len(cycled)
	for len(cycled) > 0 {
		held = slices.Concat(held, cycled)
		var more int
		kept, more, _ = withholdDeletesOfHeldWork(p.logger, kept, held, inputs, composition, mode)
		if more == 0 {
			break
		}
		withheld += more
		kept, deps, cycled = isolateDependencyCycles(kept, p.logger)
		inCycles += len(cycled)
	}

	return kept, deps, inCycles, withheld
}

// normalizeCurrentPlanActions keeps SQL as structural authority, then applies
// action-only execution semantics that cannot be represented by a single path's
// reconciliation row.
//
// Parent preservation runs first: it matches descendants by the paths SQLite
// reconciled them at, which are consistent with each other; the rebases then
// move whole subtrees, preserved parents included, to where they execute.
//
// It also returns the actions the keep-both trim left for the next pass, and
// how many of them the mode runs.
func normalizeCurrentPlanActions(
	actions []syncdomain.Action,
	remoteToLocal []graphFolderMoveBoundary,
	mode syncdomain.SyncMode,
) (normalized, trimmed []syncdomain.Action, trimmedInMode int) {
	beforeInMode := countActionsAllowedInMode(actions, mode)
	actions, trimmed = trimKeepBothDescendants(actions)
	trimmedInMode = max(0, beforeInMode-countActionsAllowedInMode(actions, mode))
	actions = preserveParentFoldersForRunnableDescendants(actions, mode)
	actions = rebaseLocalWritesToLocalFolderPositions(actions, remoteToLocal)
	return rebaseRemoteCreatesThroughLocalFolderMoves(actions), trimmed, trimmedInMode
}

// countActionsAllowedInMode counts the actions mode runs (actionAllowedInMode).
func countActionsAllowedInMode(actions []syncdomain.Action, mode syncdomain.SyncMode) int {
	n := 0
	for i := range actions {
		if actionAllowedInMode(&actions[i], mode) {
			n++
		}
	}

	return n
}

// countRenames counts the actions that put an item at a new path (moves,
// baseline re-keys, conflict copies), whose follow-up the next pass plans.
func countRenames(actions []syncdomain.Action) int {
	n := 0
	for i := range actions {
		switch actions[i].Type {
		case syncdomain.ActionLocalMove, syncdomain.ActionRemoteMove, syncdomain.ActionConflictCopy:
			n++
		case syncdomain.ActionBaselineUpdate:
			if actions[i].OldPath != "" {
				n++
			}
		case syncdomain.ActionDownload, syncdomain.ActionUpload, syncdomain.ActionLocalDelete, syncdomain.ActionRemoteDelete,
			syncdomain.ActionFolderCreate, syncdomain.ActionCleanup:
		default:
			panic(syncdomain.OutsideTheEnum(actions[i].Type))
		}
	}

	return n
}

// rebaseRemoteCreatesThroughLocalFolderMoves moves new local content under a
// folder this plan renames locally (OneDrive renamed it) to the folder's new
// path. The upload or remote folder create resolves its OneDrive parent from
// the baseline, which knows the folder only at its new path once the local
// move commits; addressed at the old local path it failed on every pass and
// held the folder's move behind it. It now runs after the move. Its view keeps
// the snapshot path, which is where freshness finds its local truth.
func rebaseRemoteCreatesThroughLocalFolderMoves(actions []syncdomain.Action) []syncdomain.Action {
	var folderMoves []graphFolderMoveBoundary
	for i := range actions {
		action := &actions[i]
		if action.Type != syncdomain.ActionLocalMove ||
			syncdomain.ResolveItemType(action.View) != syncdomain.ItemTypeFolder ||
			action.OldPath == "" {
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
	sort.Slice(folderMoves, func(i, j int) bool {
		return len(folderMoves[i].oldPrefix) > len(folderMoves[j].oldPrefix)
	})

	rebased := make([]syncdomain.Action, len(actions))
	copy(rebased, actions)
	for i := range rebased {
		action := &rebased[i]
		if createsOnSide(action, moveCoordRemote) {
			action.Path = rebaseThroughFolderMoves(action.Path, folderMoves)
		}
	}

	return rebased
}

// rebaseLocalWritesToLocalFolderPositions makes remote-origin local writes
// address the local tree as it is. They carry remote paths, but a folder that
// sits elsewhere locally (renamed by the user, or renamed online and not yet
// moved here) does not exist at its remote path: OneDrive creating photos/x
// inside docs while docs was renamed locally to renamed-docs must land at
// renamed-docs/x. Applied at the remote path, the write re-created docs/
// locally, and the next pass uploaded the stray folder. Moves are already
// composed (planner_move_compose.go), and a local move waits for the
// writes under its source, so the rebased writes run before it. Remote-side
// writes need no rebasing: they address parents by item ID.
func rebaseLocalWritesToLocalFolderPositions(actions []syncdomain.Action, remoteToLocal []graphFolderMoveBoundary) []syncdomain.Action {
	if len(remoteToLocal) == 0 {
		return actions
	}

	rebased := make([]syncdomain.Action, 0, len(actions))
	for i := range actions {
		action := actions[i]
		if action.Type == syncdomain.ActionLocalMove || !writesLocally(&action) || deletesWhatIsHere(&action) {
			rebased = append(rebased, action)
			continue
		}
		at := rebaseThroughFolderMoves(action.Path, remoteToLocal)
		if action.Type == syncdomain.ActionConflictCopy && at != action.Path {
			// A conflict copy keeps the entry here at its path, which is
			// not in the folder OneDrive's item is in: that folder sits
			// elsewhere here, where the download lands with nothing to
			// keep. Rebased there, the copy renamed nothing, or the
			// download of an earlier pass, on every pass.
			continue
		}
		action.Path = at
		rebased = append(rebased, action)
	}

	return rebased
}

// deletesWhatIsHere reports a local delete of the entry this computer has at
// the action's path. Its path is a local one, the entry it removes, not a
// remote path to rebase: where OneDrive deleted folder p and moved folder x
// onto p, the delete of p/f removes the local p/f, and rebased through x's
// remote move it would have addressed x/f, an entry nobody deleted.
func deletesWhatIsHere(action *syncdomain.Action) bool {
	return action.Type == syncdomain.ActionLocalDelete && action.View != nil && action.View.Local != nil
}

// writesLocally reports remote-origin actions that apply OneDrive's state to
// the local tree, addressed by path.
func writesLocally(action *syncdomain.Action) bool {
	switch action.Type {
	case syncdomain.ActionLocalMove, syncdomain.ActionDownload, syncdomain.ActionLocalDelete, syncdomain.ActionConflictCopy:
		return true
	case syncdomain.ActionFolderCreate:
		return action.CreateSide == syncdomain.CreateLocal
	case syncdomain.ActionUpload, syncdomain.ActionRemoteMove, syncdomain.ActionRemoteDelete, syncdomain.ActionBaselineUpdate,
		syncdomain.ActionCleanup:
		return false
	default:
		panic(syncdomain.OutsideTheEnum(action.Type))
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

// graphFolderMoveBoundary maps a folder's subtree from one path prefix to
// another.
type graphFolderMoveBoundary struct {
	oldPrefix string
	newPrefix string
}

// logMoveNotices reports composition decisions a user may want to know about.
func logMoveNotices(logger *slog.Logger, notices []moveNotice) {
	if logger == nil {
		return
	}
	for i := range notices {
		n := &notices[i]
		attrs := []any{
			slog.String("path", n.baselinePath),
			slog.String("local_path", n.localPath),
			slog.String("remote_path", n.remotePath),
		}
		switch n.kind {
		case moveNoticeDivergentRename:
			logger.Info("Renamed differently on both sides; keeping the OneDrive name and moving the local copy", attrs...)
		case moveNoticeCycle:
			logger.Info("Moves on both sides would nest the folders inside each other; keeping the OneDrive move", attrs...)
		case moveNoticeCollision:
			logger.Info("A local move collides with another item's place; keeping the OneDrive location", attrs...)
		case moveNoticeDeferred:
			logger.Debug("move waits for the next pass", append(attrs, slog.String("target_path", n.targetPath))...)
		}
	}
}

func comparisonPaths(rows []DecidedComparison) []string {
	if len(rows) == 0 {
		return nil
	}

	paths := make([]string, 0, len(rows))
	for i := range rows {
		paths = append(paths, rows[i].Path)
	}

	return paths
}

func logActionPlanSummary(logger *slog.Logger, message string, plan *syncdomain.ActionPlan) {
	if logger == nil || plan == nil {
		return
	}

	counts := CountByType(plan.Actions)
	logger.Info(message,
		slog.Int("total_actions", len(plan.Actions)),
		slog.Int("folder_creates", counts[syncdomain.ActionFolderCreate]),
		slog.Int("moves", counts[syncdomain.ActionLocalMove]+counts[syncdomain.ActionRemoteMove]),
		slog.Int("downloads", counts[syncdomain.ActionDownload]),
		slog.Int("uploads", counts[syncdomain.ActionUpload]),
		slog.Int("local_deletes", counts[syncdomain.ActionLocalDelete]),
		slog.Int("remote_deletes", counts[syncdomain.ActionRemoteDelete]),
		slog.Int("conflict_copies", counts[syncdomain.ActionConflictCopy]),
		slog.Int("baseline_updates", counts[syncdomain.ActionBaselineUpdate]),
		slog.Int("cleanups", counts[syncdomain.ActionCleanup]),
		slog.Int("deferred_folder_creates", plan.DeferredByMode.FolderCreates),
		slog.Int("deferred_moves", plan.DeferredByMode.Moves),
		slog.Int("deferred_downloads", plan.DeferredByMode.Downloads),
		slog.Int("deferred_uploads", plan.DeferredByMode.Uploads),
		slog.Int("deferred_local_deletes", plan.DeferredByMode.LocalDeletes),
		slog.Int("deferred_remote_deletes", plan.DeferredByMode.RemoteDeletes),
	)
}

func buildSQLitePathViews(
	comparisons []DecidedComparison,
	localRows []syncdomain.LocalStateRow,
	remoteRows []syncdomain.RemoteStateRow,
	baseline syncdomain.BaselineView,
	truthStatusByPath map[string]syncdomain.PathTruthStatus,
) (map[string]*syncdomain.PathView, map[string]*DecidedComparison, error) {
	localByPath := make(map[string]syncdomain.LocalStateRow, len(localRows))
	for i := range localRows {
		localByPath[localRows[i].Path] = localRows[i]
	}

	remoteByPath := make(map[string]syncdomain.RemoteStateRow, len(remoteRows))
	for i := range remoteRows {
		remoteByPath[remoteRows[i].Path] = remoteRows[i]
	}

	views := make(map[string]*syncdomain.PathView, len(comparisons))
	comparisonByPath := make(map[string]*DecidedComparison, len(comparisons))

	for i := range comparisons {
		row := comparisons[i]
		comparisonByPath[row.Path] = &comparisons[i]

		truthStatus, ok := truthStatusByPath[row.Path]
		if !ok {
			return nil, nil, fmt.Errorf("sync: missing truth status for comparison path %q", row.Path)
		}

		view := &syncdomain.PathView{
			Path:        row.Path,
			TruthStatus: truthStatus,
		}
		if row.BaselineType.Present() {
			entry, ok := baseline.GetByPath(row.Path)
			if !ok {
				return nil, nil, fmt.Errorf("sync: missing baseline entry for comparison path %q", row.Path)
			}
			view.Baseline = &entry
		}
		if row.LocalType.Present() {
			localRow, ok := localByPath[row.Path]
			if !ok {
				return nil, nil, fmt.Errorf("sync: missing local_state row for comparison path %q", row.Path)
			}
			view.Local = LocalStateFromSnapshotRow(&localRow)
		}
		if row.RemoteType.Present() {
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
	rec *syncdomain.SQLiteReconciliationRow,
	cmp *DecidedComparison,
	views map[string]*syncdomain.PathView,
) ([]syncdomain.Action, error) {
	if rec == nil || cmp == nil {
		return nil, errors.New("sync: reconciliation row requires comparison context")
	}

	view := views[rec.Path]
	if view == nil {
		return nil, fmt.Errorf("sync: missing path view for reconciliation path %q", rec.Path)
	}
	if plannerSuppressesUnavailableTruth(&view.TruthStatus) {
		return nil, nil
	}

	switch rec.ReconciliationKind {
	case syncdomain.ReconcileUnclassified:
		// PlanCurrentState refuses these rows before planning any
		// (validatePlannerKinds); a direct caller gets the same refusal.
		return nil, unclassifiedRowError(rec)
	case syncdomain.ReconcileNoop:
		return nil, nil
	case syncdomain.ReconcileBaselineRemove:
		return []syncdomain.Action{makeAction(syncdomain.ActionCleanup, view)}, nil
	case syncdomain.ReconcileFolderCreateLocal:
		return []syncdomain.Action{makeFolderCreate(view, syncdomain.CreateLocal)}, nil
	case syncdomain.ReconcileFolderCreateRemote:
		return []syncdomain.Action{makeFolderCreate(view, syncdomain.CreateRemote)}, nil
	case syncdomain.ReconcileUpload:
		if view.Local != nil && view.Local.ItemType == syncdomain.ItemTypeFolder {
			return nil, fmt.Errorf("%w: upload of local folder %s", errFolderTransferPlanned, rec.Path)
		}
		return []syncdomain.Action{makeAction(syncdomain.ActionUpload, view)}, nil
	case syncdomain.ReconcileDownload:
		if view.Remote != nil && view.Remote.ItemType == syncdomain.ItemTypeFolder {
			return nil, fmt.Errorf("%w: download of remote folder %s", errFolderTransferPlanned, rec.Path)
		}
		return []syncdomain.Action{makeAction(syncdomain.ActionDownload, view)}, nil
	case syncdomain.ReconcileLocalDelete:
		return []syncdomain.Action{makeAction(syncdomain.ActionLocalDelete, view)}, nil
	case syncdomain.ReconcileRemoteDelete:
		return []syncdomain.Action{makeAction(syncdomain.ActionRemoteDelete, view)}, nil
	case syncdomain.ReconcileBaselineUpdate:
		return []syncdomain.Action{makeAction(syncdomain.ActionBaselineUpdate, view)}, nil
	case syncdomain.ReconcileConflictEditEdit:
		return []syncdomain.Action{
			makeConflictCopyAction(view),
			makeDownloadAfterConflictCopyAction(view),
		}, nil
	case syncdomain.ReconcileConflictEditDelete:
		return []syncdomain.Action{
			makeCreateUploadAction(view),
		}, nil
	case syncdomain.ReconcileConflictCreateCreate:
		if view.Local != nil && view.Remote != nil && view.Local.ItemType != view.Remote.ItemType {
			return buildKeepBothActions(view), nil
		}
		return []syncdomain.Action{
			makeConflictCopyAction(view),
			makeDownloadAfterConflictCopyAction(view),
		}, nil
	case syncdomain.ReconcileConflictKeepBoth:
		return buildKeepBothActions(view), nil
	case syncdomain.ReconcileReplaceRemote:
		return buildReplaceRemoteActions(view), nil
	case syncdomain.ReconcileReplaceLocal:
		return buildReplaceLocalActions(view), nil
	case syncdomain.ReconcileLocalMove, syncdomain.ReconcileRemoteMove:
		// Moves are composed per item (planner_move_compose.go).
		return nil, nil
	default:
		return nil, fmt.Errorf("sync: unsupported reconciliation kind %s for %s", rec.ReconciliationKind, rec.Path)
	}
}

// buildKeepBothActions resolves a conflict between entries of different types
// at one path (D-059, D-059a): the local entry, file or folder, is renamed to
// a conflict copy (uploaded later as new content), then OneDrive's item takes
// the path as what it is: a folder is created locally, a file downloaded into
// the now-missing target. No content of either side is deleted.
// trimKeepBothDescendants removes the planned work below the path that the
// rename invalidates.
func buildKeepBothActions(view *syncdomain.PathView) []syncdomain.Action {
	actions := []syncdomain.Action{makeConflictCopyAction(view)}
	if view.Remote != nil && view.Remote.ItemType == syncdomain.ItemTypeFolder {
		return append(actions, makeFolderCreate(view, syncdomain.CreateLocal))
	}

	return append(actions, makeDownloadAfterConflictCopyAction(view))
}

// trimKeepBothDescendants keeps, below a keep-both path, only OneDrive's
// content arriving into the folder that takes the path (downloads and local
// folder creates, plus restores of the children that looked deleted locally)
// and baseline cleanup. Everything else there addressed the local entry the
// conflict copy moves away: uploads and local deletes of its children would run
// against paths that no longer hold them, and remote deletes of children the
// local side "lost" would delete OneDrive content the keep-both keeps. The next
// pass plans the conflict copy's content as new. It returns the actions it
// keeps and those it removes.
func trimKeepBothDescendants(actions []syncdomain.Action) (kept, trimmed []syncdomain.Action) {
	keepBoth := keepBothPaths(actions)
	if len(keepBoth) == 0 {
		return actions, nil
	}

	kept = actions[:0:0]
	for i := range actions {
		action := &actions[i]
		remoteIsFolder, under := keepBothAncestor(action.Path, keepBoth)
		if !under || action.Type == syncdomain.ActionCleanup {
			kept = append(kept, *action)
			continue
		}
		if replacement, keep := keepBothDescendantAction(action, remoteIsFolder); keep {
			kept = append(kept, replacement)
			continue
		}
		trimmed = append(trimmed, *action)
	}

	return kept, trimmed
}

// keepBothPaths maps each keep-both path (a conflict copy between entries of
// different types) to whether OneDrive's item there is a folder.
func keepBothPaths(actions []syncdomain.Action) map[string]bool {
	keepBoth := map[string]bool{}
	for i := range actions {
		action := &actions[i]
		if action.Type != syncdomain.ActionConflictCopy || action.View == nil || action.View.Local == nil || action.View.Remote == nil {
			continue
		}
		// A folder's conflict copy moves its whole local subtree away, like
		// one between types.
		if action.View.Local.ItemType != action.View.Remote.ItemType || action.View.Local.ItemType == syncdomain.ItemTypeFolder {
			keepBoth[action.Path] = action.View.Remote.ItemType == syncdomain.ItemTypeFolder
		}
	}

	return keepBoth
}

func keepBothAncestor(p string, keepBoth map[string]bool) (remoteIsFolder bool, under bool) {
	for dir := path.Dir(p); dir != "." && dir != "/"; dir = path.Dir(dir) {
		if isFolder, ok := keepBoth[dir]; ok {
			return isFolder, true
		}
	}

	return false, false
}

// keepBothDescendantAction decides one action below a keep-both path: content
// arriving into OneDrive's folder stays, and an unchanged child of that folder
// that looks deleted locally (the local entry at the path was a file) is
// restored, not deleted online. Anything else waits for the next pass.
func keepBothDescendantAction(action *syncdomain.Action, remoteIsFolder bool) (syncdomain.Action, bool) {
	if !remoteIsFolder {
		return syncdomain.Action{}, false
	}
	if createsOnSide(action, moveCoordLocal) {
		return *action, true
	}
	if action.Type == syncdomain.ActionRemoteDelete && action.View != nil && action.View.Remote != nil {
		return restoreRemoteChild(action.View), true
	}

	return syncdomain.Action{}, false
}

func restoreRemoteChild(view *syncdomain.PathView) syncdomain.Action {
	if view.Remote.ItemType == syncdomain.ItemTypeFolder {
		return makeFolderCreate(view, syncdomain.CreateLocal)
	}
	download := makeAction(syncdomain.ActionDownload, view)
	download.RequireMissingLocalTarget = true

	return download
}

// buildReplaceRemoteActions replaces the OneDrive item at a path whose local
// item is now of another type (or whose remote type changed while local
// content also changed): delete the old remote item, then create the local
// item as new content. buildDependencies orders the create after the delete.
//
// The create's view has no baseline or remote: it is a brand-new item, and by
// the time it dispatches the delete outcome has removed the old remote_state
// row, so remote freshness expects absence.
func buildReplaceRemoteActions(view *syncdomain.PathView) []syncdomain.Action {
	deleteAction := makeAction(syncdomain.ActionRemoteDelete, view)

	createView := &syncdomain.PathView{
		Path:        view.Path,
		Local:       view.Local,
		TruthStatus: view.TruthStatus,
	}
	if view.Local != nil && view.Local.ItemType == syncdomain.ItemTypeFolder {
		return []syncdomain.Action{deleteAction, makeFolderCreate(createView, syncdomain.CreateRemote)}
	}

	return []syncdomain.Action{deleteAction, makeCreateUploadAction(createView)}
}

// buildReplaceLocalActions replaces the local item at a path whose OneDrive
// item is now of another type while local truth is unchanged: delete the old
// local item, then materialize the remote one.
//
// The delete's view describes the old local item (ResolveItemType prefers the
// remote side, which already has the new type). local_state is refreshed only
// by observation, so the create keeps the planned local row for freshness, and
// a download requires its target to be missing, as after a conflict copy.
func buildReplaceLocalActions(view *syncdomain.PathView) []syncdomain.Action {
	deleteView := &syncdomain.PathView{
		Path:        view.Path,
		Local:       view.Local,
		Remote:      view.Remote,
		Baseline:    view.Baseline,
		TruthStatus: view.TruthStatus,
	}
	deleteAction := makeAction(syncdomain.ActionLocalDelete, deleteView)

	createView := &syncdomain.PathView{
		Path:        view.Path,
		Local:       view.Local,
		Remote:      view.Remote,
		TruthStatus: view.TruthStatus,
	}
	if view.Remote != nil && view.Remote.ItemType == syncdomain.ItemTypeFolder {
		return []syncdomain.Action{deleteAction, makeFolderCreate(createView, syncdomain.CreateLocal)}
	}

	download := makeAction(syncdomain.ActionDownload, createView)
	download.RequireMissingLocalTarget = true

	return []syncdomain.Action{deleteAction, download}
}

func partitionCurrentActionsForMode(
	actions []syncdomain.Action, mode syncdomain.SyncMode,
) ([]syncdomain.Action, syncdomain.DeferredCounts) {
	admitted := make([]syncdomain.Action, 0, len(actions))
	var deferred syncdomain.DeferredCounts
	for i := range actions {
		if actionAllowedInMode(&actions[i], mode) {
			admitted = append(admitted, actions[i])
		} else {
			deferred.AddAction(&actions[i])
		}
	}

	return admitted, deferred
}

// leaveOutNewLocalItems admits what a plan that releases its local tree may
// run (PlannerMountContext.ReleasesLocalTree): it creates nothing new locally.
// A download or a local folder create of an item that has no baseline row and
// no local entry is left out, so the final drain never fetches an item it has
// not synced, and an entry an earlier release took is never downloaded back.
// A folder that an admitted action below it needs stays: a synced item
// OneDrive moved into a new folder moves there, and the release then takes
// both. What is left out is neither follow-up work nor work left: the tree it
// would join is going. The plan logs its count once, at Debug.
func (p *Planner) leaveOutNewLocalItems(actions []syncdomain.Action, mode syncdomain.SyncMode) []syncdomain.Action {
	needed := foldersNeededByAdmittedActions(actions, mode)
	kept := make([]syncdomain.Action, 0, len(actions))
	leftOut := 0
	for i := range actions {
		action := &actions[i]
		if createsNewLocalItem(action) && (action.Type != syncdomain.ActionFolderCreate || !needed[action.Path]) {
			leftOut++
			continue
		}
		kept = append(kept, *action)
	}
	if leftOut > 0 && p.logger != nil {
		p.logger.Debug("plan of a tree being released leaves out items new to it",
			slog.Int("left_out", leftOut))
	}

	return kept
}

// createsNewLocalItem reports a download or a local folder create of an item
// with no baseline row and no local entry.
func createsNewLocalItem(action *syncdomain.Action) bool {
	if action.View == nil || action.View.Baseline != nil || action.View.Local != nil {
		return false
	}

	return action.Type == syncdomain.ActionDownload ||
		(action.Type == syncdomain.ActionFolderCreate && action.CreateSide == syncdomain.CreateLocal)
}

// foldersNeededByAdmittedActions is every folder above an action the mode
// admits that needs its parent to exist (anything but a delete or cleanup,
// as runnableSubtreeActionRequiresParent says) and is not itself a new local
// item.
func foldersNeededByAdmittedActions(actions []syncdomain.Action, mode syncdomain.SyncMode) map[string]bool {
	needed := map[string]bool{}
	for i := range actions {
		action := &actions[i]
		if createsNewLocalItem(action) || isDeleteAction(action) || !actionAllowedInMode(action, mode) {
			continue
		}
		for dir := path.Dir(action.Path); dir != "." && dir != "/" && !needed[dir]; dir = path.Dir(dir) {
			needed[dir] = true
		}
	}

	return needed
}

func LocalStateFromSnapshotRow(row *syncdomain.LocalStateRow) *syncdomain.LocalState {
	if row == nil {
		return nil
	}

	return &syncdomain.LocalState{
		Name:             path.Base(row.Path),
		ItemType:         row.ItemType,
		Size:             row.Size,
		Hash:             row.Hash,
		Mtime:            row.Mtime,
		LocalCtime:       row.Ctime,
		LocalDevice:      row.LocalDevice,
		LocalInode:       row.LocalInode,
		LocalBirthTime:   row.LocalBirthTime,
		LocalHasIdentity: row.LocalHasIdentity,
	}
}

func remoteStateFromSnapshotRow(row *syncdomain.RemoteStateRow) *syncdomain.RemoteState {
	if row == nil {
		return nil
	}

	return &syncdomain.RemoteState{
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
