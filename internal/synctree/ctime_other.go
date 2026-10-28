//go:build !linux && !darwin

package synctree

import "io/fs"

// ChangeTimeFromFileInfo reports no ctime on platforms the sync engine does
// not support; callers then never trust a cached hash by metadata alone.
func ChangeTimeFromFileInfo(fs.FileInfo) (int64, bool) {
	return 0, false
}
