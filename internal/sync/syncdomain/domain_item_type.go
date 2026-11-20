package syncdomain

// ResolveItemType determines the item type by checking Remote, Local,
// then Baseline. Defaults to ItemTypeFile if none provide a type.
//
// Special case: when the delta API reports a deleted item, it may strip
// the folder facet — making item.IsFolder=false even for folders. When
// Remote is deleted and its ItemType is the default (ItemTypeFile), we
// fall through to Baseline which has the correct type from when the item
// was alive. This ensures folder deletes are correctly identified for
// dependency ordering in buildDependencies/addChildDeleteDeps.
func ResolveItemType(view *PathView) ItemType {
	if view == nil {
		return ItemTypeFile
	}

	if view.Remote != nil {
		// When the remote item is deleted, the delta API may omit the folder
		// facet, causing ItemType to default to ItemTypeFile. If a baseline
		// exists with a non-file type, prefer it — the baseline recorded the
		// correct type when the item was still alive.
		if view.Remote.IsDeleted &&
			view.Remote.ItemType == ItemTypeFile &&
			view.Baseline != nil &&
			view.Baseline.ItemType != ItemTypeFile {
			return view.Baseline.ItemType
		}

		return view.Remote.ItemType
	}

	if view.Local != nil {
		return view.Local.ItemType
	}

	if view.Baseline != nil {
		return view.Baseline.ItemType
	}

	return ItemTypeFile
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
