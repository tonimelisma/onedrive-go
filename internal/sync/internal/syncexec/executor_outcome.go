// The outcomes the executor builds from an action's result: failures,
// folder creates and moves, filled from the baseline where the action
// left a side unchanged.

package syncexec

import (
	"errors"
	"log/slog"
	"net/http"
	"os"

	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// failedOutcome builds an ActionOutcome for a failed action.
//
// The failure is logged at Debug only. Its user-facing report is the sync
// console's activity line ("Failed to upload X: reason", or the --verbose
// "will retry" line), which also knows to hide superseded and retried
// failures; a Warn here printed every failure a second time.
func (e *Executor) failedOutcome(action *syncdomain.Action, actionType syncdomain.ActionType, err error) syncdomain.ActionOutcome {
	e.logger.Debug("action failed",
		slog.String("action", actionType.String()),
		slog.String("path", action.Path),
		slog.Any("error", err),
	)

	return syncdomain.ActionOutcome{
		Action:  actionType,
		Success: false,
		Error:   err,
		Path:    action.Path,
		DriveID: e.resolveDriveID(action),
		ItemID:  action.ItemID,
	}
}

func (e *Executor) failedOutcomeWithFailure(
	action *syncdomain.Action,
	actionType syncdomain.ActionType,
	err error,
	failurePath string,
	failureCapability syncdomain.PermissionCapability,
) syncdomain.ActionOutcome {
	outcome := e.failedOutcome(action, actionType, err)
	if failurePath != "" {
		outcome.FailurePath = failurePath
	}
	outcome.FailureCapability = failureCapability

	return outcome
}

func inferFailureCapabilityFromError(
	err error,
	localCapability syncdomain.PermissionCapability,
	remoteCapability syncdomain.PermissionCapability,
) syncdomain.PermissionCapability {
	switch {
	case errors.Is(err, os.ErrPermission):
		return localCapability
	case errors.Is(err, graph.ErrForbidden):
		return remoteCapability
	case syncdomain.ExtractHTTPStatus(err) == http.StatusForbidden:
		return remoteCapability
	default:
		return syncdomain.PermissionCapabilityUnknown
	}
}

// folderOutcome builds a successful ActionOutcome for a local folder create
// of folder, the rooted spelling on disk of action.Path.
func (e *Executor) folderOutcome(action *syncdomain.Action, folder string) syncdomain.ActionOutcome {
	o := syncdomain.ActionOutcome{
		Action:   syncdomain.ActionFolderCreate,
		Success:  true,
		Path:     action.Path,
		DriveID:  e.resolveDriveID(action),
		ItemID:   action.ItemID,
		ItemType: syncdomain.ItemTypeFolder,
		ParentID: e.resolvedParentIDForOutcome(action, nil),
	}

	if action.View != nil && action.View.Remote != nil {
		o.ItemID = action.View.Remote.ItemID
		o.ETag = action.View.Remote.ETag
	}
	e.observeLocalIdentity(&o, folder)

	return o
}

// observeLocalIdentity records the filesystem identity and ctime of the entry
// this executor just materialized at o.Path, so a later local rename of it can
// pair as a move and the next scan can trust the recorded hash (R-2.2.4). The
// identity is always marked observed: an entry the executor just (re)created
// must never inherit a stale baseline identity or ctime, so a failed lookup
// records "unknown" rather than falling back, and unknown forces a re-read.
// local is the entry's rooted spelling on disk (R-2.13.2).
func (e *Executor) observeLocalIdentity(o *syncdomain.ActionOutcome, local string) {
	o.LocalIdentityObserved = true
	o.LocalCtime = 0
	info, err := e.syncTree.Lstat(local)
	if err != nil {
		e.logger.Debug("local identity unavailable after materializing entry",
			slog.String("path", o.Path),
			slog.Any("error", err),
		)
		return
	}
	o.LocalCtime = syncdomain.FileChangeTime(info)
	identity, ok := synctree.IdentityFromFileInfo(info)
	if !ok {
		return
	}
	o.LocalDevice = identity.Device
	o.LocalInode = identity.Inode
	o.LocalBirthTime = identity.BirthTime
	o.LocalHasIdentity = true
}

// moveOutcome builds a successful ActionOutcome for a move action.
//
// The baseline row it publishes must describe only what was observed or proven:
//   - ActionRemoteMove propagates a local move, so the local half comes from the
//     local truth the planner observed at the destination (MovedLocal), never
//     from the source baseline. An unknown destination hash stays unknown.
//   - ActionLocalMove propagates a remote move. The rename preserves content,
//     size, mtime and filesystem identity, and validateLocalMovePrecondition
//     proved the source still matched its baseline, so the source baseline's
//     local half is exactly what now sits at the destination.
//   - If the remote item's content changed as well as its path, the local copy
//     still holds the old content: the remote half keeps the source baseline's
//     content facts so the next pass sees the remote edit and downloads it.
func (e *Executor) moveOutcome(action *syncdomain.Action) syncdomain.ActionOutcome {
	o := syncdomain.ActionOutcome{
		Action:   action.Type,
		Success:  true,
		Path:     action.Path,
		OldPath:  action.OldPath,
		DriveID:  e.resolveDriveID(action),
		ItemID:   action.ItemID,
		ParentID: e.resolvedParentIDForOutcome(action, nil),
	}

	if action.View == nil {
		return o
	}
	baseline := action.View.Baseline

	if remote := action.View.Remote; remote != nil {
		o.ETag = remote.ETag
		o.RemoteHash = remote.Hash
		o.RemoteSize = remote.Size
		o.RemoteSizeKnown = true
		o.RemoteMtime = remote.Mtime
		// A move publishes only what it changed: the path. Remote content
		// that changed since the baseline is still pending in either
		// direction. Recording it here (a local rename racing a remote
		// edit of the same file) marked the edit synced while the local
		// file kept the old bytes: a silent, permanent divergence.
		if syncdomain.RemoteContentDiffersFromBaseline(remote, baseline) {
			o.ETag = baseline.ETag
			o.RemoteHash = baseline.RemoteHash
			o.RemoteSize = baseline.RemoteSize
			o.RemoteSizeKnown = baseline.RemoteSizeKnown
			o.RemoteMtime = baseline.RemoteMtime
		}
	}

	// A local move's view describes what sat at the destination before the
	// move (normally nothing, or the file it replaces); the moved item's local
	// half is the source baseline's, which the rename preserves.
	local := action.View.Local
	if action.Type == syncdomain.ActionLocalMove {
		local = nil
	}
	if action.Type == syncdomain.ActionRemoteMove && action.MovedLocal != nil {
		local = action.MovedLocal
	}
	if local != nil {
		o.LocalHash = local.Hash
		o.LocalSize = local.Size
		o.LocalSizeKnown = true
		o.LocalMtime = local.Mtime
		o.LocalCtime = local.LocalCtime
		o.LocalDevice = local.LocalDevice
		o.LocalInode = local.LocalInode
		o.LocalBirthTime = local.LocalBirthTime
		o.LocalHasIdentity = local.LocalHasIdentity
		o.LocalIdentityObserved = true
		fillRemoteOutcomeFromBaseline(&o, baseline, action.View.Remote != nil)
	} else {
		fillOutcomeFromBaseline(&o, baseline, action.View.Remote != nil)
	}
	o.ItemType = movedItemType(action)

	return o
}

// movedItemType is the type of the item a move moved, read from the side
// known to be observed, in one order and with no "a file means unknown": the
// remote observation, then the source baseline (a rename preserves the type),
// then the moved item's observed local truth. It differs from
// ResolveItemType only in skipping the view's local side, which for a move
// describes what sat at the destination before it, not the moved item.
func movedItemType(action *syncdomain.Action) syncdomain.ItemType {
	view := action.View
	switch {
	case view.Remote != nil:
		return view.Remote.ItemType
	case view.Baseline != nil:
		return view.Baseline.ItemType
	case action.MovedLocal != nil:
		return action.MovedLocal.ItemType
	default:
		return syncdomain.ResolveItemType(nil)
	}
}

// plannedRemoteItemType is the remote item type the planner saw for action.
func plannedRemoteItemType(action *syncdomain.Action) syncdomain.ItemType {
	if planned := plannedRemoteStateForPrecondition(action); planned != nil {
		return planned.ItemType
	}

	return syncdomain.ResolveItemType(action.View)
}

func fillOutcomeFromBaseline(o *syncdomain.ActionOutcome, baseline *syncdomain.BaselineEntry, remoteObserved bool) {
	if baseline == nil {
		return
	}

	if o.LocalHash == "" {
		o.LocalHash = baseline.LocalHash
	}
	if !o.LocalSizeKnown {
		o.LocalSize = baseline.LocalSize
		o.LocalSizeKnown = baseline.LocalSizeKnown
	}
	if o.LocalMtime == 0 {
		o.LocalMtime = baseline.LocalMtime
	}
	if !o.LocalIdentityObserved {
		o.LocalCtime = baseline.LocalCtime
		o.LocalDevice = baseline.LocalDevice
		o.LocalInode = baseline.LocalInode
		o.LocalBirthTime = baseline.LocalBirthTime
		o.LocalHasIdentity = baseline.LocalHasIdentity
	}
	fillRemoteOutcomeFromBaseline(o, baseline, remoteObserved)
}

// fillRemoteOutcomeFromBaseline fills only unknown remote-side facts. It is
// used on its own when the local half was observed, because an observed but
// unknown local hash must stay unknown rather than inherit the baseline's.
// The remote modification time is filled only when the view did not observe
// the remote side, as in a publication (fillMutationFromBaselineDefaults): an
// observed item with no time is recorded with none, which is what the
// planner's comparison reads from the remote row.
func fillRemoteOutcomeFromBaseline(o *syncdomain.ActionOutcome, baseline *syncdomain.BaselineEntry, remoteObserved bool) {
	if baseline == nil {
		return
	}

	if o.RemoteHash == "" {
		o.RemoteHash = baseline.RemoteHash
	}
	if !o.RemoteSizeKnown {
		o.RemoteSize = baseline.RemoteSize
		o.RemoteSizeKnown = baseline.RemoteSizeKnown
	}
	if !syncdomain.StoredTimeIsSet(o.RemoteMtime) && !remoteObserved {
		o.RemoteMtime = baseline.RemoteMtime
	}
	if o.ETag == "" {
		o.ETag = baseline.ETag
	}
}
