package graph

import "strings"

// itemExactRootRelativePath returns the fully reconstructed root-relative path
// when Graph provided enough information to know the parent path exactly.
func itemExactRootRelativePath(item *Item) (string, bool) {
	if item == nil || (item.ParentPath == "" && !item.ParentPathKnown) {
		return "", false
	}
	if item.ParentPath == "" {
		return item.Name, true
	}

	return item.ParentPath + "/" + item.Name, true
}

// itemBestEffortRootRelativePath returns the most specific path representation
// we currently have for an item. When Graph omitted parentReference.path we can
// only trust the leaf name, so callers must not treat this as exact unless
// itemExactRootRelativePath returned ok=true.
func itemBestEffortRootRelativePath(item *Item) string {
	if item == nil {
		return ""
	}

	if exactPath, ok := itemExactRootRelativePath(item); ok {
		return exactPath
	}

	return item.Name
}

func pathLeaf(remotePath string) string {
	lastSlash := strings.LastIndex(remotePath, "/")
	if lastSlash == -1 {
		return remotePath
	}

	return remotePath[lastSlash+1:]
}
