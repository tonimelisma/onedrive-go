package sync

func ActionItemType(action *Action) ItemType {
	if action == nil {
		return ItemTypeFile
	}
	if action.View != nil {
		if action.View.Remote != nil && action.View.Remote.ItemType != ItemTypeFile {
			return action.View.Remote.ItemType
		}
		if action.View.Baseline != nil && action.View.Baseline.ItemType != ItemTypeFile {
			return action.View.Baseline.ItemType
		}
		if action.View.Local != nil && action.View.Local.ItemType != ItemTypeFile {
			return action.View.Local.ItemType
		}
	}

	return ItemTypeFile
}

func MutationFromActionOutcome(o *ActionOutcome) *BaselineMutation {
	if o == nil {
		return nil
	}
	if o.Action == ActionConflictCopy {
		return nil
	}

	return &BaselineMutation{
		Action:                o.Action,
		Success:               o.Success,
		Path:                  o.Path,
		OldPath:               o.OldPath,
		DriveID:               o.DriveID,
		ItemID:                o.ItemID,
		ParentID:              o.ParentID,
		ItemType:              o.ItemType,
		LocalHash:             o.LocalHash,
		RemoteHash:            o.RemoteHash,
		LocalSize:             o.LocalSize,
		LocalSizeKnown:        o.LocalSizeKnown,
		RemoteSize:            o.RemoteSize,
		RemoteSizeKnown:       o.RemoteSizeKnown,
		LocalMtime:            o.LocalMtime,
		RemoteMtime:           o.RemoteMtime,
		LocalCtime:            o.LocalCtime,
		LocalDevice:           o.LocalDevice,
		LocalInode:            o.LocalInode,
		LocalBirthTime:        o.LocalBirthTime,
		LocalHasIdentity:      o.LocalHasIdentity,
		LocalIdentityObserved: o.LocalIdentityObserved,
		ETag:                  o.ETag,
		KeptLocally:           o.KeptLocally,
		ExemptFromDeleteHold:  o.DeleteConfirmed,
	}
}
