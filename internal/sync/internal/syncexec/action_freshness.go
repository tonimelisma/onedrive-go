package syncexec

import (
	"context"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

type actionFreshnessInputs struct {
	LocalTruthComplete bool
	RemoteTruthKnown   bool

	LocalAtView          *syncdomain.LocalStateRow
	LocalAtViewFound     bool
	LocalAtMovePeer      *syncdomain.LocalStateRow
	LocalAtMovePeerFound bool

	RemoteAtView          *syncdomain.RemoteStateRow
	RemoteAtViewFound     bool
	RemoteAtMovePeer      *syncdomain.RemoteStateRow
	RemoteAtMovePeerFound bool

	RemoteByID      *syncdomain.RemoteStateRow
	RemoteByIDFound bool
}

type actionFreshnessDecision struct {
	Fresh  bool
	Reason string
	Source actionFreshnessSource
}

type actionFreshnessSource int

const (
	actionFreshnessSourceUnknown actionFreshnessSource = iota
	actionFreshnessSourceLocalTruth
	actionFreshnessSourceRemoteTruth
)

// ExecutorStore is the executor's view of the sync store: the reads that
// check an action is still fresh and the commit of its outcome. *SyncStore
// implements it; the engine hands it over (executorStoreOf).
type ExecutorStore interface {
	CommitMutation(ctx context.Context, outcome *syncdomain.BaselineMutation) error
	GetLocalStateByPath(ctx context.Context, path string) (*syncdomain.LocalStateRow, bool, error)
	GetRemoteStateByID(ctx context.Context, driveID driveid.ID, itemID string) (*syncdomain.RemoteStateRow, bool, error)
	GetRemoteStateByPath(ctx context.Context, path string, driveID driveid.ID) (*syncdomain.RemoteStateRow, bool, error)
	Load(ctx context.Context) (syncdomain.BaselineView, error)
	ReadObservationState(ctx context.Context) (*syncdomain.ObservationState, error)
}

func EvaluateActionFreshnessFromStore(
	ctx context.Context,
	store ExecutorStore,
	action *syncdomain.Action,
) (actionFreshnessDecision, error) {
	// Worker and admission validation fail closed on cancellation. A stopping
	// run's completion never asks: it admits nothing, so its frontier
	// completes as shutdown without a freshness check.
	select {
	case <-ctx.Done():
		return actionFreshnessDecision{}, fmt.Errorf("sync: validating action freshness after cancellation: %w", ctx.Err())
	default:
	}
	if store == nil || action == nil || actionSkipsFreshnessValidation(action) {
		return freshAction(), nil
	}
	if !actionHasPlannerView(action) {
		return actionFreshnessDecision{}, fmt.Errorf(
			"sync: validating action freshness: %s %q missing planner view",
			action.Type,
			action.Path,
		)
	}

	inputs, err := loadActionFreshnessInputs(ctx, store, action)
	if err != nil {
		return actionFreshnessDecision{}, err
	}

	return evaluateActionFreshness(action, inputs), nil
}

func loadActionFreshnessInputs(
	ctx context.Context,
	store ExecutorStore,
	action *syncdomain.Action,
) (actionFreshnessInputs, error) {
	inputs := actionFreshnessInputs{}

	state, err := store.ReadObservationState(ctx)
	if err != nil {
		return inputs, fmt.Errorf("sync: reading observation state for action freshness: %w", err)
	}
	if state != nil {
		inputs.LocalTruthComplete = state.LocalTruthComplete
		inputs.RemoteTruthKnown = !state.ContentDriveID.IsZero() || state.Cursor != ""
	}

	if err := loadLocalActionFreshnessInputs(ctx, store, action, &inputs); err != nil {
		return inputs, err
	}
	if err := loadRemoteActionFreshnessInputs(ctx, store, action, &inputs); err != nil {
		return inputs, err
	}

	return inputs, nil
}

func loadLocalActionFreshnessInputs(
	ctx context.Context,
	store ExecutorStore,
	action *syncdomain.Action,
	inputs *actionFreshnessInputs,
) error {
	if !inputs.LocalTruthComplete {
		return nil
	}

	if actionUsesLocalFreshness(action) {
		row, found, err := store.GetLocalStateByPath(ctx, localFreshnessPath(action))
		if err != nil {
			return fmt.Errorf("sync: reading local truth for action freshness: %w", err)
		}
		inputs.LocalAtView = row
		inputs.LocalAtViewFound = found
	}

	if peerPath, ok := localMovePeerPath(action); ok {
		row, found, err := store.GetLocalStateByPath(ctx, peerPath)
		if err != nil {
			return fmt.Errorf("sync: reading local move peer for action freshness: %w", err)
		}
		inputs.LocalAtMovePeer = row
		inputs.LocalAtMovePeerFound = found
	}

	return nil
}

func loadRemoteActionFreshnessInputs(
	ctx context.Context,
	store ExecutorStore,
	action *syncdomain.Action,
	inputs *actionFreshnessInputs,
) error {
	if !inputs.RemoteTruthKnown {
		return nil
	}

	if actionUsesRemoteFreshness(action) {
		row, found, err := loadPlannedRemoteTruth(ctx, store, action)
		if err != nil {
			return err
		}
		inputs.RemoteAtView = row
		inputs.RemoteAtViewFound = found
	}

	if peerPath, ok := movePeerPath(action); ok {
		row, found, err := store.GetRemoteStateByPath(ctx, peerPath, driveid.ID{})
		if err != nil {
			return fmt.Errorf("sync: reading remote move peer for action freshness: %w", err)
		}
		inputs.RemoteAtMovePeer = row
		inputs.RemoteAtMovePeerFound = found
	}

	if actionNeedsRemoteIdentityFreshness(action) {
		row, found, err := store.GetRemoteStateByID(ctx, driveid.ID{}, action.ItemID)
		if err != nil {
			return fmt.Errorf("sync: reading remote identity for action freshness: %w", err)
		}
		inputs.RemoteByID = row
		inputs.RemoteByIDFound = found
	}

	return nil
}

// loadPlannedRemoteTruth reads the remote truth an action was planned
// against. A remote item the plan knew is looked up by item ID: its path can
// legitimately change during the pass, when a folder move above it commits
// and re-paths the subtree. Looked up by path, a download or local move into
// a folder renamed earlier in the same pass was declared stale and replanned
// on the next pass. Absence (a planned create) is still checked by path.
func loadPlannedRemoteTruth(ctx context.Context, store ExecutorStore, action *syncdomain.Action) (*syncdomain.RemoteStateRow, bool, error) {
	if planned := action.View.Remote; planned != nil && !planned.IsDeleted && planned.ItemID != "" {
		row, found, err := store.GetRemoteStateByID(ctx, driveid.ID{}, planned.ItemID)
		if err != nil {
			return nil, false, fmt.Errorf("sync: reading remote truth for action freshness: %w", err)
		}
		return row, found, nil
	}

	row, found, err := store.GetRemoteStateByPath(ctx, syncdomain.ActionViewPath(action), driveid.ID{})
	if err != nil {
		return nil, false, fmt.Errorf("sync: reading remote truth for action freshness: %w", err)
	}

	return row, found, nil
}

func evaluateActionFreshness(action *syncdomain.Action, inputs actionFreshnessInputs) actionFreshnessDecision {
	if action == nil || actionSkipsFreshnessValidation(action) {
		return freshAction()
	}
	if !actionHasPlannerView(action) {
		return staleAction("%s action is missing planner view", action.Path)
	}

	if stale := evaluateLocalActionFreshness(action, inputs); !stale.Fresh {
		return stale
	}
	if stale := evaluateRemoteActionFreshness(action, inputs); !stale.Fresh {
		return stale
	}
	if stale := evaluateMoveEndpointFreshness(action, inputs); !stale.Fresh {
		return stale
	}
	if stale := evaluateRemoteIdentityFreshness(action, inputs); !stale.Fresh {
		return stale
	}

	return freshAction()
}

func evaluateLocalActionFreshness(action *syncdomain.Action, inputs actionFreshnessInputs) actionFreshnessDecision {
	if !inputs.LocalTruthComplete || !actionUsesLocalFreshness(action) {
		return freshAction()
	}

	planned := action.View.Local
	viewPath := localFreshnessPath(action)
	if planned == nil {
		if inputs.LocalAtViewFound {
			return staleLocalAction("%s has current local truth but was absent when planned", viewPath)
		}
		return freshAction()
	}
	if !inputs.LocalAtViewFound || inputs.LocalAtView == nil {
		return staleLocalAction("%s is missing from current local truth", viewPath)
	}
	if !localRowMatchesPlanned(inputs.LocalAtView, planned) {
		return staleLocalAction("%s local truth changed since planning", viewPath)
	}

	return freshAction()
}

func evaluateRemoteActionFreshness(action *syncdomain.Action, inputs actionFreshnessInputs) actionFreshnessDecision {
	if !inputs.RemoteTruthKnown || !actionUsesRemoteFreshness(action) {
		return freshAction()
	}

	planned := action.View.Remote
	viewPath := syncdomain.ActionViewPath(action)
	if planned == nil || planned.IsDeleted {
		if inputs.RemoteAtViewFound {
			return staleRemoteAction("%s has current remote truth but was absent when planned", viewPath)
		}
		return freshAction()
	}
	if !inputs.RemoteAtViewFound || inputs.RemoteAtView == nil {
		return staleRemoteAction("%s is missing from current remote truth", viewPath)
	}
	if !remoteRowMatchesPlannedForAction(action, inputs.RemoteAtView, planned) {
		return staleRemoteAction("%s remote truth changed since planning", viewPath)
	}

	return freshAction()
}

func evaluateMoveEndpointFreshness(action *syncdomain.Action, inputs actionFreshnessInputs) actionFreshnessDecision {
	switch action.Type {
	case syncdomain.ActionLocalMove:
		return evaluateLocalMoveEndpointFreshness(action, inputs)
	case syncdomain.ActionRemoteMove:
		return evaluateRemoteMoveEndpointFreshness(action, inputs)
	case syncdomain.ActionDownload,
		syncdomain.ActionUpload,
		syncdomain.ActionLocalDelete,
		syncdomain.ActionRemoteDelete,
		syncdomain.ActionFolderCreate,
		syncdomain.ActionConflictCopy,
		syncdomain.ActionBaselineUpdate,
		syncdomain.ActionCleanup:
		return freshAction()
	default:
		panic(syncdomain.OutsideTheEnum(action.Type))
	}
}

func evaluateLocalMoveEndpointFreshness(action *syncdomain.Action, inputs actionFreshnessInputs) actionFreshnessDecision {
	if inputs.LocalTruthComplete && action.OldPath != "" {
		if !inputs.LocalAtMovePeerFound || inputs.LocalAtMovePeer == nil {
			return staleLocalAction("%s local move source is missing from current local truth", action.OldPath)
		}
		// The source must still be the synced item; its content may have
		// changed (D-053), which the rename carries and the next pass uploads.
		if action.View.Baseline != nil && !syncdomain.LocalRowIsBaselineItem(inputs.LocalAtMovePeer, action.View.Baseline) {
			return staleLocalAction("%s local move source is no longer the synced item", action.OldPath)
		}
	}
	// The moved item back at its old path means the remote move was undone.
	// Another item there is a chain or a swap: it took the path the move gave
	// up and plans its own move into it once this one has vacated it.
	if inputs.RemoteTruthKnown && action.OldPath != "" && inputs.RemoteAtMovePeerFound &&
		(inputs.RemoteAtMovePeer == nil || inputs.RemoteAtMovePeer.ItemID == action.ItemID) {
		return staleRemoteAction("%s remote move source reappeared in current remote truth", action.OldPath)
	}

	return freshAction()
}

func evaluateRemoteMoveEndpointFreshness(action *syncdomain.Action, inputs actionFreshnessInputs) actionFreshnessDecision {
	if inputs.LocalTruthComplete && action.Path != "" && action.Path != syncdomain.ActionViewPath(action) {
		peer, _ := localMovePeerPath(action)
		if !inputs.LocalAtMovePeerFound || inputs.LocalAtMovePeer == nil {
			return staleLocalAction("%s local move destination is missing from current local truth", peer)
		}
		if action.MovedLocal != nil && !localRowMatchesPlanned(inputs.LocalAtMovePeer, action.MovedLocal) {
			return staleLocalAction("%s local move destination changed since planning", peer)
		}
	}
	// The same item at the destination means the move already happened (the
	// other side made the same rename, or an earlier attempt's outcome was
	// lost): the executor records it without a mutation (D-050).
	if inputs.RemoteTruthKnown && action.Path != "" && action.Path != syncdomain.ActionViewPath(action) &&
		inputs.RemoteAtMovePeerFound && (inputs.RemoteAtMovePeer == nil || inputs.RemoteAtMovePeer.ItemID != action.ItemID) {
		return staleRemoteAction("%s remote move destination already exists in current remote truth", action.Path)
	}

	return freshAction()
}

func evaluateRemoteIdentityFreshness(action *syncdomain.Action, inputs actionFreshnessInputs) actionFreshnessDecision {
	if !inputs.RemoteTruthKnown || !actionNeedsRemoteIdentityFreshness(action) {
		return freshAction()
	}
	if !inputs.RemoteByIDFound || inputs.RemoteByID == nil {
		return staleRemoteAction("%s remote item %s is missing from current remote truth", syncdomain.ActionViewPath(action), action.ItemID)
	}
	if inputs.RemoteByID.Path != expectedRemoteIdentityPath(action) {
		return staleRemoteAction("%s remote item %s moved to %s", syncdomain.ActionViewPath(action), action.ItemID, inputs.RemoteByID.Path)
	}
	if !remoteRowMatchesPlannedForAction(action, inputs.RemoteByID, action.View.Remote) {
		return staleRemoteAction("%s remote item %s changed since planning", syncdomain.ActionViewPath(action), action.ItemID)
	}

	return freshAction()
}

func actionSkipsFreshnessValidation(action *syncdomain.Action) bool {
	return action.Type == syncdomain.ActionBaselineUpdate || action.Type == syncdomain.ActionCleanup
}

func actionUsesLocalFreshness(action *syncdomain.Action) bool {
	if !actionHasPlannerView(action) {
		return false
	}
	if action.Type == syncdomain.ActionDownload && action.RequireMissingLocalTarget {
		return false
	}

	return true
}

func actionUsesRemoteFreshness(action *syncdomain.Action) bool {
	return actionHasPlannerView(action)
}

func actionNeedsRemoteIdentityFreshness(action *syncdomain.Action) bool {
	return actionHasPlannerView(action) &&
		action.View.Remote != nil &&
		!action.View.Remote.IsDeleted &&
		action.ItemID != ""
}

func actionHasPlannerView(action *syncdomain.Action) bool {
	return action != nil && action.View != nil && syncdomain.ActionViewPath(action) != ""
}

// localFreshnessPath is where freshness reads an action's planned local truth
// in local_state: its view's path, except for a download composed with the
// local move that carries the copy here to its path (MovedLocalPath).
// local_state is observation-owned and does not follow the plan's own moves,
// so the copy is still recorded at the move's source.
func localFreshnessPath(action *syncdomain.Action) string {
	if movedIntoPlaceDownload(action) {
		return action.MovedLocalPath
	}

	return syncdomain.ActionViewPath(action)
}

// movedIntoPlaceDownload reports a download of OneDrive's edit onto the copy
// a local move of the same plan puts at its path (the planner's
// composedRemoteEditDownload).
func movedIntoPlaceDownload(action *syncdomain.Action) bool {
	return action != nil && action.Type == syncdomain.ActionDownload && action.MovedLocalPath != ""
}

// localMovePeerPath is where freshness reads a move's other endpoint in
// local_state: the snapshot path of a remote move's item, which differs from
// the destination when an earlier move of the plan places its parent.
func localMovePeerPath(action *syncdomain.Action) (string, bool) {
	if action != nil && action.Type == syncdomain.ActionRemoteMove && action.MovedLocalPath != "" {
		return action.MovedLocalPath, actionHasPlannerView(action)
	}

	return movePeerPath(action)
}

func movePeerPath(action *syncdomain.Action) (string, bool) {
	if !actionHasPlannerView(action) {
		return "", false
	}

	switch action.Type {
	case syncdomain.ActionLocalMove:
		return action.OldPath, action.OldPath != ""
	case syncdomain.ActionRemoteMove:
		return action.Path, action.Path != "" && action.Path != syncdomain.ActionViewPath(action)
	case syncdomain.ActionDownload,
		syncdomain.ActionUpload,
		syncdomain.ActionLocalDelete,
		syncdomain.ActionRemoteDelete,
		syncdomain.ActionFolderCreate,
		syncdomain.ActionConflictCopy,
		syncdomain.ActionBaselineUpdate,
		syncdomain.ActionCleanup:
		return "", false
	default:
		panic(syncdomain.OutsideTheEnum(action.Type))
	}
}

func expectedRemoteIdentityPath(action *syncdomain.Action) string {
	if action == nil {
		return ""
	}

	if action.Type == syncdomain.ActionRemoteMove {
		if action.OldPath != "" {
			return action.OldPath
		}
	}
	if action.Type == syncdomain.ActionLocalMove {
		if action.MovedRemotePath != "" {
			return action.MovedRemotePath
		}
		if action.Path != "" {
			return action.Path
		}
	}

	return syncdomain.ActionViewPath(action)
}

func localRowMatchesPlanned(row *syncdomain.LocalStateRow, planned *syncdomain.LocalState) bool {
	if row == nil || planned == nil {
		return row == nil && planned == nil
	}
	if row.ItemType != planned.ItemType {
		return false
	}
	if rowIdentity, rowKnown := row.FileIdentity(); rowKnown {
		if plannedIdentity, plannedKnown := planned.FileIdentity(); plannedKnown &&
			!synctree.SameIdentity(rowIdentity, plannedIdentity) {
			return false
		}
	}
	if planned.ItemType == syncdomain.ItemTypeFolder {
		return true
	}
	if row.Hash != "" || planned.Hash != "" {
		return row.Hash == planned.Hash && row.Size == planned.Size
	}

	return row.Size == planned.Size && row.Mtime == planned.Mtime
}

// LocalRowIsBaselineItem reports whether a local row is still the synced item:
// the same type and, when both sides know it, the same filesystem identity.
func remoteRowMatchesPlanned(row *syncdomain.RemoteStateRow, planned *syncdomain.RemoteState) bool {
	if row == nil || planned == nil {
		return row == nil && planned == nil
	}
	if !remoteIdentityMatchesPlanned(row, planned) {
		return false
	}

	return remoteContentMatchesPlanned(row, planned)
}

func remoteRowMatchesPlannedForAction(action *syncdomain.Action, row *syncdomain.RemoteStateRow, planned *syncdomain.RemoteState) bool {
	if plannedPostRemoteMoveContentTransfer(action) {
		return remoteRowMatchesPostRemoteMoveTransfer(row, planned)
	}

	return remoteRowMatchesPlanned(row, planned)
}

func remoteRowMatchesPostRemoteMoveTransfer(row *syncdomain.RemoteStateRow, planned *syncdomain.RemoteState) bool {
	if row == nil || planned == nil {
		return row == nil && planned == nil
	}
	if !remoteIdentityMatchesPlannedExceptETag(row, planned) {
		return false
	}

	return remoteContentMatchesPlanned(row, planned)
}

func plannedPostRemoteMoveContentTransfer(action *syncdomain.Action) bool {
	if !contentTransferHasMovedBaseline(action) {
		return false
	}

	return plannedPostMoveRemoteMatchesBaseline(action.View.Remote, action.View.Baseline)
}

// contentTransferHasMovedBaseline reports an upload or download planned at
// the path a remote move of the same plan gives the item (its baseline is
// still at the source): the transfer runs after the move, whose new eTag is
// no change of the content the transfer was planned against. Content drift
// still makes it stale (remoteRowMatchesPostRemoteMoveTransfer).
func contentTransferHasMovedBaseline(action *syncdomain.Action) bool {
	if action == nil || action.View == nil ||
		(action.Type != syncdomain.ActionUpload && action.Type != syncdomain.ActionDownload) {
		return false
	}
	if action.View.Baseline == nil || action.View.Remote == nil {
		return false
	}
	if action.View.Baseline.Path == "" || action.View.Baseline.Path == syncdomain.ActionViewPath(action) {
		return false
	}

	return true
}

func plannedPostMoveRemoteMatchesBaseline(remote *syncdomain.RemoteState, baseline *syncdomain.BaselineEntry) bool {
	if baseline == nil || remote == nil {
		return false
	}
	if baseline.ItemType != syncdomain.ItemTypeFile || remote.ItemType != syncdomain.ItemTypeFile {
		return false
	}
	if baseline.ItemID != "" && remote.ItemID != "" && baseline.ItemID != remote.ItemID {
		return false
	}
	if !baseline.DriveID.IsZero() && !remote.DriveID.IsZero() &&
		!baseline.DriveID.Equal(remote.DriveID) {
		return false
	}

	return true
}

func remoteContentMatchesPlanned(row *syncdomain.RemoteStateRow, planned *syncdomain.RemoteState) bool {
	if planned.ItemType == syncdomain.ItemTypeFolder {
		return true
	}
	if row.Hash != "" || planned.Hash != "" {
		return row.Hash == planned.Hash && row.Size == planned.Size
	}
	if row.Size != planned.Size {
		return false
	}
	if syncdomain.StoredTimeIsSet(row.Mtime) && syncdomain.StoredTimeIsSet(planned.Mtime) {
		return row.Mtime == planned.Mtime
	}

	return true
}

func remoteIdentityMatchesPlanned(row *syncdomain.RemoteStateRow, planned *syncdomain.RemoteState) bool {
	if !remoteIdentityMatchesPlannedExceptETag(row, planned) {
		return false
	}
	// Folder eTags churn with every descendant change; see
	// remoteRowMatchesLivePreconditionForAction.
	if planned.ItemType != syncdomain.ItemTypeFolder && planned.ETag != "" && row.ETag != planned.ETag {
		return false
	}

	return true
}

func remoteIdentityMatchesPlannedExceptETag(row *syncdomain.RemoteStateRow, planned *syncdomain.RemoteState) bool {
	if planned.ItemID != "" && row.ItemID != planned.ItemID {
		return false
	}
	if !planned.DriveID.IsZero() && !row.DriveID.Equal(planned.DriveID) {
		return false
	}
	if row.ItemType != planned.ItemType {
		return false
	}

	return true
}

func freshAction() actionFreshnessDecision {
	return actionFreshnessDecision{Fresh: true}
}

func staleAction(format string, args ...any) actionFreshnessDecision {
	return actionFreshnessDecision{
		Fresh:  false,
		Reason: fmt.Sprintf(format, args...),
	}
}

func staleLocalAction(format string, args ...any) actionFreshnessDecision {
	decision := staleAction(format, args...)
	decision.Source = actionFreshnessSourceLocalTruth
	return decision
}

func staleRemoteAction(format string, args ...any) actionFreshnessDecision {
	decision := staleAction(format, args...)
	decision.Source = actionFreshnessSourceRemoteTruth
	return decision
}
