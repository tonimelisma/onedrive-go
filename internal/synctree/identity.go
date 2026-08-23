package synctree

import (
	"fmt"
	"io/fs"
	"syscall"
)

// FileIdentity is a platform filesystem identity for a directory entry under a
// sync tree. It is useful for local rename detection only; it is not a content
// identity and must not be used to authorize cross-directory moves.
type FileIdentity struct {
	Device uint64
	Inode  uint64
}

func IdentityFromFileInfo(info fs.FileInfo) (FileIdentity, bool) {
	if info == nil {
		return FileIdentity{}, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return FileIdentity{}, false
	}
	identity := FileIdentity{
		Device: statFieldToUint64(stat.Dev),
		Inode:  statFieldToUint64(stat.Ino),
	}
	if identity.Device == 0 && identity.Inode == 0 {
		return FileIdentity{}, false
	}

	return identity, true
}

// statFieldToUint64 normalizes a syscall.Stat_t numeric field to uint64.
//
// The signedness and width of Dev and Ino vary by platform (Dev is int32 on
// darwin and the OpenBSDs, uint64 on linux and freebsd), which previously
// forced one build-tagged file per platform purely to keep the conversion
// necessary on every target. Doing it in a generic function keeps a single
// implementation that compiles everywhere and stays honest for `unconvert`.
func statFieldToUint64[T ~int16 | ~uint16 | ~int32 | ~uint32 | ~int64 | ~uint64](value T) uint64 {
	return uint64(value)
}

func SameIdentity(a FileIdentity, b FileIdentity) bool {
	return a.Device == b.Device && a.Inode == b.Inode
}

func (r *Root) IdentityNoFollow(rel string) (FileIdentity, error) {
	info, err := r.Lstat(rel)
	if err != nil {
		return FileIdentity{}, fmt.Errorf("stating identity %s: %w", rel, err)
	}
	identity, ok := IdentityFromFileInfo(info)
	if !ok {
		return FileIdentity{}, fmt.Errorf("file info for %s has no stable device/inode identity", rel)
	}

	return identity, nil
}
