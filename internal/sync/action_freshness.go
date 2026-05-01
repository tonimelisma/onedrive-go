package sync

import (
	"context"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

type actionFreshnessInputs struct {
	LocalTruthComplete bool
	RemoteTruthKnown   bool

	LocalAtView          *LocalStateRow
	LocalAtViewFound     bool
	LocalAtMovePeer      *LocalStateRow
	LocalAtMovePeerFound bool

	RemoteAtView          *RemoteStateRow
	RemoteAtViewFound     bool
	RemoteAtMovePeer      *RemoteStateRow
	RemoteAtMovePeerFound bool

	RemoteByID      *RemoteStateRow
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

	if peerPath, ok := movePeerPath(action); ok {
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
		viewPath := actionViewPath(action)
		row, found, err := store.GetRemoteStateByPath(ctx, viewPath, driveid.ID{})
		if err != nil {
			return fmt.Errorf("sync: reading remote truth for action freshness: %w", err)
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
		if action.View.Baseline != nil && !localRowMatchesBaseline(inputs.LocalAtMovePeer, action.View.Baseline) {
			return staleLocalAction("%s local move source changed since planning", action.OldPath)
		}
	}
	if inputs.RemoteTruthKnown && action.OldPath != "" && inputs.RemoteAtMovePeerFound {
		return staleRemoteAction("%s remote move source reappeared in current remote truth", action.OldPath)
	}

	return freshAction()
}

func evaluateRemoteMoveEndpointFreshness(action *Action, inputs actionFreshnessInputs) actionFreshnessDecision {
	if inputs.LocalTruthComplete && action.Path != "" && action.Path != actionViewPath(action) {
		if !inputs.LocalAtMovePeerFound || inputs.LocalAtMovePeer == nil {
			return staleLocalAction("%s local move destination is missing from current local truth", action.Path)
		}
	}
	if inputs.RemoteTruthKnown && action.Path != "" && action.Path != actionViewPath(action) && inputs.RemoteAtMovePeerFound {
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

func actionViewPath(action *Action) string {
	if action != nil && action.View != nil && action.View.Path != "" {
		return action.View.Path
	}
	if action != nil {
		return action.Path
	}

	return ""
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
		if action.Path != "" {
			return action.Path
		}
	}

	return actionViewPath(action)
}

func localRowMatchesPlanned(row *LocalStateRow, planned *LocalState) bool {
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

func localRowMatchesBaseline(row *LocalStateRow, baseline *BaselineEntry) bool {
	if row == nil || baseline == nil {
		return row == nil && baseline == nil
	}
	if row.ItemType != baseline.ItemType {
		return false
	}
	if !localRowIdentityMatchesBaseline(row, baseline) {
		return false
	}
	if baseline.ItemType == ItemTypeFolder {
		return true
	}

	return localFileRowMatchesBaseline(row, baseline)
}

func localRowIdentityMatchesBaseline(row *LocalStateRow, baseline *BaselineEntry) bool {
	if !baseline.LocalHasIdentity || !row.LocalHasIdentity {
		return true
	}

	return row.LocalDevice == baseline.LocalDevice && row.LocalInode == baseline.LocalInode
}

func localFileRowMatchesBaseline(row *LocalStateRow, baseline *BaselineEntry) bool {
	if row.Hash != "" || baseline.LocalHash != "" {
		return row.Hash == baseline.LocalHash &&
			(!baseline.LocalSizeKnown || row.Size == baseline.LocalSize)
	}
	if baseline.LocalSizeKnown && row.Size != baseline.LocalSize {
		return false
	}

	return row.Mtime != 0 && row.Mtime == baseline.LocalMtime
}

func remoteRowMatchesPlanned(row *RemoteStateRow, planned *RemoteState) bool {
	if row == nil || planned == nil {
		return row == nil && planned == nil
	}
	if !remoteIdentityMatchesPlanned(row, planned) {
		return false
	}

	return remoteContentMatchesPlanned(row, planned)
}

func remoteRowMatchesPlannedForAction(action *Action, row *RemoteStateRow, planned *RemoteState) bool {
	if plannedPostRemoteMoveContentUpload(action) {
		return remoteRowMatchesPostRemoteMoveUpload(row, planned)
	}

	return remoteRowMatchesPlanned(row, planned)
}

func remoteRowMatchesPostRemoteMoveUpload(row *RemoteStateRow, planned *RemoteState) bool {
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

func plannedPostMoveRemoteMatchesBaseline(remote *RemoteState, baseline *BaselineEntry) bool {
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

func remoteContentMatchesPlanned(row *RemoteStateRow, planned *RemoteState) bool {
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

func remoteIdentityMatchesPlanned(row *RemoteStateRow, planned *RemoteState) bool {
	if !remoteIdentityMatchesPlannedExceptETag(row, planned) {
		return false
	}
	if planned.ETag != "" && row.ETag != planned.ETag {
		return false
	}

	return true
}

func remoteIdentityMatchesPlannedExceptETag(row *RemoteStateRow, planned *RemoteState) bool {
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
