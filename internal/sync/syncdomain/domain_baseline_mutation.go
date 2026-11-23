package syncdomain

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
