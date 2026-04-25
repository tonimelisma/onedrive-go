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
		Device: statDeviceID(stat),
		Inode:  stat.Ino,
	}
	if identity.Device == 0 && identity.Inode == 0 {
		return FileIdentity{}, false
	}

	return identity, true
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
