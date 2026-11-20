package sync

// RemoteItem is one item a remote observer reports: the row it stores and,
// when the item moved, the path that row had before. The converter and the
// orphan finders build it; the old path is never stored.
type RemoteItem struct {
	ObservedItem
	OldPath string
}
