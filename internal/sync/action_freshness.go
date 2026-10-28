package sync

import (
	"context"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

type actionFreshnessInputs struct {
	LocalTruthComplete bool
	RemoteTruthKnown   bool

	LocalAtView          *localStateRow
	LocalAtViewFound     bool
	LocalAtMovePeer      *localStateRow
	LocalAtMovePeerFound bool

	RemoteAtView          *remoteStateRow
	RemoteAtViewFound     bool
	RemoteAtMovePeer      *remoteStateRow
	RemoteAtMovePeerFound bool

	RemoteByID      *remoteStateRow
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

type shutdownFreshnessBypassKey struct{}

func contextWithShutdownFreshnessBypass(ctx context.Context) context.Context {
	return context.WithValue(ctx, shutdownFreshnessBypassKey{}, true)
}

func shutdownFreshnessBypass(ctx context.Context) bool {
	value := ctx.Value(shutdownFreshnessBypassKey{})
	enabled, ok := value.(bool)
	return ok && enabled
}

func evaluateActionFreshnessFromStore(
	ctx context.Context,
	store *SyncStore,
	action *Action,
) (actionFreshnessDecision, error) {
	select {
	case <-ctx.Done():
		// Shutdown completion owns collapsing newly-ready frontier after
		// cancellation. That collapse does not dispatch side effects; ordinary
		// worker/admission validation must still fail closed on cancellation.
		if shutdownFreshnessBypass(ctx) {
			return freshAction(), nil
		}
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
	store *SyncStore,
	action *Action,
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
	store *SyncStore,
	action *Action,
	inputs *actionFreshnessInputs,
) error {
	if !inputs.LocalTruthComplete {
		return nil
	}

	if actionUsesLocalFreshness(action) {
		viewPath := actionViewPath(action)
		row, found, err := store.GetLocalStateByPath(ctx, viewPath)
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
	store *SyncStore,
	action *Action,
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
func loadPlannedRemoteTruth(ctx context.Context, store *SyncStore, action *Action) (*remoteStateRow, bool, error) {
	if planned := action.View.Remote; planned != nil && !planned.IsDeleted && planned.ItemID != "" {
		row, found, err := store.GetRemoteStateByID(ctx, driveid.ID{}, planned.ItemID)
		if err != nil {
			return nil, false, fmt.Errorf("sync: reading remote truth for action freshness: %w", err)
		}
		return row, found, nil
	}

	row, found, err := store.GetRemoteStateByPath(ctx, actionViewPath(action), driveid.ID{})
	if err != nil {
		return nil, false, fmt.Errorf("sync: reading remote truth for action freshness: %w", err)
	}

	return row, found, nil
}

func evaluateActionFreshness(action *Action, inputs actionFreshnessInputs) actionFreshnessDecision {
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

func evaluateLocalActionFreshness(action *Action, inputs actionFreshnessInputs) actionFreshnessDecision {
	if !inputs.LocalTruthComplete || !actionUsesLocalFreshness(action) {
		return freshAction()
	}

	planned := action.View.Local
	viewPath := actionViewPath(action)
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

func evaluateRemoteActionFreshness(action *Action, inputs actionFreshnessInputs) actionFreshnessDecision {
	if !inputs.RemoteTruthKnown || !actionUsesRemoteFreshness(action) {
		return freshAction()
	}

	planned := action.View.Remote
	viewPath := actionViewPath(action)
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

func evaluateMoveEndpointFreshness(action *Action, inputs actionFreshnessInputs) actionFreshnessDecision {
	switch action.Type {
	case ActionLocalMove:
		return evaluateLocalMoveEndpointFreshness(action, inputs)
	case ActionRemoteMove:
		return evaluateRemoteMoveEndpointFreshness(action, inputs)
	case ActionDownload,
		ActionUpload,
		ActionLocalDelete,
		ActionRemoteDelete,
		ActionFolderCreate,
		ActionConflictCopy,
		ActionBaselineUpdate,
		ActionCleanup:
		return freshAction()
	}

	return freshAction()
}

func evaluateLocalMoveEndpointFreshness(action *Action, inputs actionFreshnessInputs) actionFreshnessDecision {
	if inputs.LocalTruthComplete && action.OldPath != "" {
		if !inputs.LocalAtMovePeerFound || inputs.LocalAtMovePeer == nil {
			return staleLocalAction("%s local move source is missing from current local truth", action.OldPath)
		}
		// The source must still be the synced item; its content may have
		// changed (D-053), which the rename carries and the next pass uploads.
		if action.View.Baseline != nil && !localRowIsBaselineItem(inputs.LocalAtMovePeer, action.View.Baseline) {
			return staleLocalAction("%s local move source is no longer the synced item", action.OldPath)
		}
	}
	if inputs.RemoteTruthKnown && action.OldPath != "" && inputs.RemoteAtMovePeerFound {
		return staleRemoteAction("%s remote move source reappeared in current remote truth", action.OldPath)
	}

	return freshAction()
}

func evaluateRemoteMoveEndpointFreshness(action *Action, inputs actionFreshnessInputs) actionFreshnessDecision {
	if inputs.LocalTruthComplete && action.Path != "" && action.Path != actionViewPath(action) {
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
	if inputs.RemoteTruthKnown && action.Path != "" && action.Path != actionViewPath(action) &&
		inputs.RemoteAtMovePeerFound && (inputs.RemoteAtMovePeer == nil || inputs.RemoteAtMovePeer.ItemID != action.ItemID) {
		return staleRemoteAction("%s remote move destination already exists in current remote truth", action.Path)
	}

	return freshAction()
}

func evaluateRemoteIdentityFreshness(action *Action, inputs actionFreshnessInputs) actionFreshnessDecision {
	if !inputs.RemoteTruthKnown || !actionNeedsRemoteIdentityFreshness(action) {
		return freshAction()
	}
	if !inputs.RemoteByIDFound || inputs.RemoteByID == nil {
		return staleRemoteAction("%s remote item %s is missing from current remote truth", actionViewPath(action), action.ItemID)
	}
	if inputs.RemoteByID.Path != expectedRemoteIdentityPath(action) {
		return staleRemoteAction("%s remote item %s moved to %s", actionViewPath(action), action.ItemID, inputs.RemoteByID.Path)
	}
	if !remoteRowMatchesPlannedForAction(action, inputs.RemoteByID, action.View.Remote) {
		return staleRemoteAction("%s remote item %s changed since planning", actionViewPath(action), action.ItemID)
	}

	return freshAction()
}

func actionSkipsFreshnessValidation(action *Action) bool {
	return action.Type == ActionBaselineUpdate || action.Type == ActionCleanup
}

func actionUsesLocalFreshness(action *Action) bool {
	if !actionHasPlannerView(action) {
		return false
	}
	if action.Type == ActionDownload && action.RequireMissingLocalTarget {
		return false
	}

	return true
}

func actionUsesRemoteFreshness(action *Action) bool {
	return actionHasPlannerView(action)
}

func actionNeedsRemoteIdentityFreshness(action *Action) bool {
	return actionHasPlannerView(action) &&
		action.View.Remote != nil &&
		!action.View.Remote.IsDeleted &&
		action.ItemID != ""
}

func actionHasPlannerView(action *Action) bool {
	return action != nil && action.View != nil && actionViewPath(action) != ""
}

// localMovePeerPath is where freshness reads a move's other endpoint in
// local_state: the snapshot path of a remote move's item, which differs from
// the destination when an earlier move of the plan places its parent.
func localMovePeerPath(action *Action) (string, bool) {
	if action != nil && action.Type == ActionRemoteMove && action.MovedLocalPath != "" {
		return action.MovedLocalPath, actionHasPlannerView(action)
	}

	return movePeerPath(action)
}

func movePeerPath(action *Action) (string, bool) {
	if !actionHasPlannerView(action) {
		return "", false
	}

	switch action.Type {
	case ActionLocalMove:
		return action.OldPath, action.OldPath != ""
	case ActionRemoteMove:
		return action.Path, action.Path != "" && action.Path != actionViewPath(action)
	case ActionDownload,
		ActionUpload,
		ActionLocalDelete,
		ActionRemoteDelete,
		ActionFolderCreate,
		ActionConflictCopy,
		ActionBaselineUpdate,
		ActionCleanup:
		return "", false
	}

	return "", false
}

func expectedRemoteIdentityPath(action *Action) string {
	if action == nil {
		return ""
	}

	if action.Type == ActionRemoteMove {
		if action.OldPath != "" {
			return action.OldPath
		}
	}
	if action.Type == ActionLocalMove {
		if action.MovedRemotePath != "" {
			return action.MovedRemotePath
		}
		if action.Path != "" {
			return action.Path
		}
	}

	return actionViewPath(action)
}

func localRowMatchesPlanned(row *localStateRow, planned *localState) bool {
	if row == nil || planned == nil {
		return row == nil && planned == nil
	}
	if row.ItemType != planned.ItemType {
		return false
	}
	if planned.LocalHasIdentity && row.LocalHasIdentity &&
		(row.LocalDevice != planned.LocalDevice || row.LocalInode != planned.LocalInode) {
		return false
	}
	if planned.ItemType == ItemTypeFolder {
		return true
	}
	if row.Hash != "" || planned.Hash != "" {
		return row.Hash == planned.Hash && row.Size == planned.Size
	}

	return row.Size == planned.Size && row.Mtime == planned.Mtime
}

// localRowIsBaselineItem reports whether a local row is still the synced item:
// the same type and, when both sides know it, the same filesystem identity.
func localRowIsBaselineItem(row *localStateRow, baseline *BaselineEntry) bool {
	if row == nil || baseline == nil {
		return row == nil && baseline == nil
	}

	return row.ItemType == baseline.ItemType && localRowIdentityMatchesBaseline(row, baseline)
}

func localRowIdentityMatchesBaseline(row *localStateRow, baseline *BaselineEntry) bool {
	if !baseline.LocalHasIdentity || !row.LocalHasIdentity {
		return true
	}

	return row.LocalDevice == baseline.LocalDevice && row.LocalInode == baseline.LocalInode
}

func remoteRowMatchesPlanned(row *remoteStateRow, planned *remoteState) bool {
	if row == nil || planned == nil {
		return row == nil && planned == nil
	}
	if !remoteIdentityMatchesPlanned(row, planned) {
		return false
	}

	return remoteContentMatchesPlanned(row, planned)
}

func remoteRowMatchesPlannedForAction(action *Action, row *remoteStateRow, planned *remoteState) bool {
	if plannedPostRemoteMoveContentUpload(action) {
		return remoteRowMatchesPostRemoteMoveUpload(row, planned)
	}

	return remoteRowMatchesPlanned(row, planned)
}

func remoteRowMatchesPostRemoteMoveUpload(row *remoteStateRow, planned *remoteState) bool {
	if row == nil || planned == nil {
		return row == nil && planned == nil
	}
	if !remoteIdentityMatchesPlannedExceptETag(row, planned) {
		return false
	}

	return remoteContentMatchesPlanned(row, planned)
}

func plannedPostRemoteMoveContentUpload(action *Action) bool {
	if !actionUploadHasMovedBaseline(action) {
		return false
	}

	return plannedPostMoveRemoteMatchesBaseline(action.View.Remote, action.View.Baseline)
}

func actionUploadHasMovedBaseline(action *Action) bool {
	if action == nil || action.Type != ActionUpload || action.View == nil {
		return false
	}
	if action.View.Baseline == nil || action.View.Remote == nil {
		return false
	}
	if action.View.Baseline.Path == "" || action.View.Baseline.Path == actionViewPath(action) {
		return false
	}

	return true
}

func plannedPostMoveRemoteMatchesBaseline(remote *remoteState, baseline *BaselineEntry) bool {
	if baseline == nil || remote == nil {
		return false
	}
	if baseline.ItemType != ItemTypeFile || remote.ItemType != ItemTypeFile {
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

func remoteContentMatchesPlanned(row *remoteStateRow, planned *remoteState) bool {
	if planned.ItemType == ItemTypeFolder {
		return true
	}
	if row.Hash != "" || planned.Hash != "" {
		return row.Hash == planned.Hash && row.Size == planned.Size
	}
	if row.Size != planned.Size {
		return false
	}
	if row.Mtime != 0 && planned.Mtime != 0 {
		return row.Mtime == planned.Mtime
	}

	return true
}

func remoteIdentityMatchesPlanned(row *remoteStateRow, planned *remoteState) bool {
	if !remoteIdentityMatchesPlannedExceptETag(row, planned) {
		return false
	}
	// Folder eTags churn with every descendant change; see
	// remoteRowMatchesLivePreconditionForAction.
	if planned.ItemType != ItemTypeFolder && planned.ETag != "" && row.ETag != planned.ETag {
		return false
	}

	return true
}

func remoteIdentityMatchesPlannedExceptETag(row *remoteStateRow, planned *remoteState) bool {
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
