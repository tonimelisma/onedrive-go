package syncdomain

// ResolveItemType is the type of the item a view describes, read from the
// side known to be observed: the remote observation, then the local one, then
// the baseline (the type the item had when it was last synced, for a path
// neither side holds any more, such as a cleanup's). ItemTypeFile is a type
// like any other here, never "unknown": a side that is present was observed
// with its type. No planner view carries a remote tombstone (the planner reads
// remote truth from remote_state rows, which hold live items only), so a
// deleted remote observation needs no rule of its own. A nil view is a file.
func ResolveItemType(view *PathView) ItemType {
	switch {
	case view == nil:
		return ItemTypeFile
	case view.Remote != nil:
		return view.Remote.ItemType
	case view.Local != nil:
		return view.Local.ItemType
	case view.Baseline != nil:
		return view.Baseline.ItemType
	default:
		return ItemTypeFile
	}
}

// DeletedItemType is the type of the item a delete removes: the side it
// deletes from, when that side is known. It differs from ResolveItemType only
// across a type change, where the two sides disagree and the view's preferred
// (remote) type would misdescribe a local delete.
func DeletedItemType(action *Action) ItemType {
	if action == nil || action.View == nil {
		return ResolveItemType(nil)
	}
	switch action.Type {
	case ActionLocalDelete:
		if action.View.Local != nil {
			return action.View.Local.ItemType
		}
	case ActionRemoteDelete:
		if action.View.Remote != nil {
			return action.View.Remote.ItemType
		}
	case ActionDownload, ActionUpload, ActionLocalMove, ActionRemoteMove,
		ActionFolderCreate, ActionConflictCopy, ActionBaselineUpdate, ActionCleanup:
	default:
		panic(OutsideTheEnum(action.Type))
	}

	return ResolveItemType(action.View)
}
