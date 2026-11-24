// The baseline mutation an executed action publishes
// (PublicationMutationFromAction), filled from the stored baseline where the
// action left a side unchanged.

package syncstore

import (
	"errors"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

func publicationDriveIDForAction(action *syncdomain.Action, defaultDriveID driveid.ID) driveid.ID {
	if action == nil {
		return defaultDriveID
	}
	if !action.DriveID.IsZero() {
		return action.DriveID
	}
	if action.View != nil {
		if action.View.Remote != nil && !action.View.Remote.DriveID.IsZero() {
			return action.View.Remote.DriveID
		}
		if action.View.Baseline != nil && !action.View.Baseline.DriveID.IsZero() {
			return action.View.Baseline.DriveID
		}
	}

	return defaultDriveID
}

// fillMutationFromBaselineDefaults fills from the baseline the facts a
// publication's view left unknown. A modification time, and the remote's
// eTag, are filled only for a side the view did not observe: an observed 0 is
// a local file dated exactly the epoch or a remote item with no time, an
// observed empty eTag is a remote item with none, and either way it is the
// value the planner's comparison reads from current truth, so the baseline's
// value in its place would differ from current truth on every pass. Content
// (hashes and sizes) is filled only from a baseline of the publication's own
// item type: what a file held is no fact about the folder that replaced it.
func fillMutationFromBaselineDefaults(mutation *syncdomain.BaselineMutation, baseline *syncdomain.BaselineEntry, remoteObserved bool) {
	if mutation == nil || baseline == nil {
		return
	}

	if mutation.ItemID == "" {
		mutation.ItemID = baseline.ItemID
	}
	if mutation.ParentID == "" {
		mutation.ParentID = baseline.ParentID
	}
	if baseline.ItemType == mutation.ItemType {
		fillMutationContentFromBaseline(mutation, baseline)
	}
	if mutation.LocalMtime == 0 && !mutation.LocalIdentityObserved {
		mutation.LocalMtime = baseline.LocalMtime
	}
	if !mutation.LocalIdentityObserved {
		mutation.LocalCtime = baseline.LocalCtime
		mutation.LocalDevice = baseline.LocalDevice
		mutation.LocalInode = baseline.LocalInode
		mutation.LocalBirthTime = baseline.LocalBirthTime
		mutation.LocalHasIdentity = baseline.LocalHasIdentity
	}
	if !syncdomain.StoredTimeIsSet(mutation.RemoteMtime) && !remoteObserved {
		mutation.RemoteMtime = baseline.RemoteMtime
	}
	if mutation.ETag == "" && !remoteObserved {
		mutation.ETag = baseline.ETag
	}
}

// fillMutationContentFromBaseline fills the hashes and sizes the view left
// unknown from a baseline of the same item type.
func fillMutationContentFromBaseline(mutation *syncdomain.BaselineMutation, baseline *syncdomain.BaselineEntry) {
	if mutation.LocalHash == "" {
		mutation.LocalHash = baseline.LocalHash
	}
	if !mutation.LocalSizeKnown {
		mutation.LocalSize = baseline.LocalSize
		mutation.LocalSizeKnown = baseline.LocalSizeKnown
	}
	if mutation.RemoteHash == "" {
		mutation.RemoteHash = baseline.RemoteHash
	}
	if !mutation.RemoteSizeKnown {
		mutation.RemoteSize = baseline.RemoteSize
		mutation.RemoteSizeKnown = baseline.RemoteSizeKnown
	}
}

func PublicationMutationFromAction(action *syncdomain.Action, defaultDriveID driveid.ID) (*syncdomain.BaselineMutation, error) {
	if action == nil {
		return nil, errors.New("syncstore: building publication mutation: nil action")
	}
	if !action.Type.IsMember() || !syncdomain.IsPublicationOnlyActionType(action.Type) {
		return nil, fmt.Errorf("syncstore: building publication mutation: %s is not publication-only", action.Type.String())
	}

	mutation := &syncdomain.BaselineMutation{
		Action:  action.Type,
		Success: true,
		Path:    action.Path,
		OldPath: action.OldPath,
		DriveID: publicationDriveIDForAction(action, defaultDriveID),
		ItemID:  action.ItemID,
		// The observed type stands where the baseline recorded another: a
		// folder both sides replaced with the same file is that file, and
		// keeping the folder's type plans this update on every pass.
		ItemType: syncdomain.ResolveItemType(action.View),
	}

	if action.View != nil {
		if action.View.Remote != nil {
			mutation.ItemID = action.View.Remote.ItemID
			mutation.RemoteHash = action.View.Remote.Hash
			mutation.RemoteSize = action.View.Remote.Size
			mutation.RemoteSizeKnown = true
			mutation.RemoteMtime = action.View.Remote.Mtime
			mutation.ETag = action.View.Remote.ETag
			if !action.View.Remote.DriveID.IsZero() {
				mutation.DriveID = action.View.Remote.DriveID
			}
		}

		if action.View.Local != nil {
			mutation.LocalHash = action.View.Local.Hash
			mutation.LocalSize = action.View.Local.Size
			mutation.LocalSizeKnown = true
			mutation.LocalMtime = action.View.Local.Mtime
			mutation.LocalCtime = action.View.Local.LocalCtime
			mutation.LocalDevice = action.View.Local.LocalDevice
			mutation.LocalInode = action.View.Local.LocalInode
			mutation.LocalBirthTime = action.View.Local.LocalBirthTime
			mutation.LocalHasIdentity = action.View.Local.LocalHasIdentity
			mutation.LocalIdentityObserved = true
		}

		fillMutationFromBaselineDefaults(mutation, action.View.Baseline, action.View.Remote != nil)
		if action.OldPath != "" && action.OldPath != action.Path {
			keepPendingContentAcrossRekey(mutation, action.View)
		}
	}

	return mutation, nil
}

// keepPendingContentAcrossRekey makes a baseline re-key publish only the path
// (D-050, as a move outcome does). Both sides hold the item at the new path,
// but either may also have edited it; recording that content as synced would
// lose the edit silently, so a side whose content differs from the baseline
// keeps the baseline's content facts and the next pass transfers it.
func keepPendingContentAcrossRekey(mutation *syncdomain.BaselineMutation, view *syncdomain.PathView) {
	baseline := view.Baseline
	if baseline == nil {
		return
	}
	if syncdomain.RemoteContentDiffersFromBaseline(view.Remote, baseline) {
		mutation.ETag = baseline.ETag
		mutation.RemoteHash = baseline.RemoteHash
		mutation.RemoteSize = baseline.RemoteSize
		mutation.RemoteSizeKnown = baseline.RemoteSizeKnown
		mutation.RemoteMtime = baseline.RemoteMtime
	}
	if syncdomain.LocalFileDiffersFromBaseline(view.Local, baseline) {
		mutation.LocalHash = baseline.LocalHash
		mutation.LocalSize = baseline.LocalSize
		mutation.LocalSizeKnown = baseline.LocalSizeKnown
		mutation.LocalMtime = baseline.LocalMtime
		mutation.LocalCtime = baseline.LocalCtime
	}
}
