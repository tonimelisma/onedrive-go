//go:build unix

package synctree

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// RootIdentity fingerprints the directory a sync tree is rooted at, so a
// persisted sync state can prove on a later start that it is still looking at
// the same folder. An unmounted disk leaves an empty mount-point directory, a
// wiped tmpfs or a re-pointed symlink leaves a different directory, and every
// one of them scans as "everything was deleted" unless the folder itself is
// checked first.
//
// FileSystemID is statfs(2)'s f_fsid. On ext4 and btrfs it is derived from the
// filesystem UUID, so it survives the device-number reshuffle that happens
// when disks enumerate in a different order after a reboot; Device does not.
// Filesystems that report no fsid leave it zero and fall back to Device.
type RootIdentity struct {
	Device       uint64
	Inode        uint64
	FileSystemID uint64
}

// SameRoot reports whether two root fingerprints name the same directory.
//
// The inode must always match. The filesystem is matched by fsid when both
// sides carry one, because the device number of the same disk can change
// between boots; otherwise by device number. A false mismatch is safe (the
// engine refuses to sync until the user rebinds), a false match is not, so
// nothing here is looser than the old (device, inode) rule except the one
// reshuffle case fsid exists to cover.
func SameRoot(a RootIdentity, b RootIdentity) bool {
	if a.Inode != b.Inode {
		return false
	}
	if a.FileSystemID != 0 && b.FileSystemID != 0 {
		return a.FileSystemID == b.FileSystemID
	}

	return a.Device == b.Device
}

// RootIdentityFromFileIdentity widens a (device, inode) identity, such as the
// parent-issued identity of a shortcut child root, into a root fingerprint
// that compares by device number.
func RootIdentityFromFileIdentity(identity FileIdentity) RootIdentity {
	return RootIdentity{Device: identity.Device, Inode: identity.Inode}
}

// ErrNoStableRootIdentity reports a filesystem that exposes no inode identity
// for the sync folder, so the folder cannot be fingerprinted at all.
var ErrNoStableRootIdentity = errors.New("this filesystem reports no stable identity for the sync folder")

// RootIdentity fingerprints the sync folder through one open descriptor, so
// the inode and the filesystem id describe the same directory even if the
// path is re-pointed while it is read.
func (r *Root) RootIdentity() (RootIdentity, error) {
	dir, err := r.Open(".")
	if err != nil {
		return RootIdentity{}, err
	}
	defer dir.Close() //nolint:errcheck // read-only descriptor; nothing to flush

	info, err := dir.Stat()
	if err != nil {
		return RootIdentity{}, fmt.Errorf("stating sync folder %s: %w", r.dir, err)
	}
	identity, ok := IdentityFromFileInfo(info)
	if !ok {
		return RootIdentity{}, fmt.Errorf("fingerprinting sync folder %s: %w", r.dir, ErrNoStableRootIdentity)
	}

	var fsStat unix.Statfs_t
	if err := unix.Fstatfs(int(dir.Fd()), &fsStat); err != nil {
		return RootIdentity{}, fmt.Errorf("reading filesystem of sync folder %s: %w", r.dir, err)
	}

	return RootIdentity{
		Device:       identity.Device,
		Inode:        identity.Inode,
		FileSystemID: fileSystemIDFromStatfs(&fsStat),
	}, nil
}

func fileSystemIDFromStatfs(stat *unix.Statfs_t) uint64 {
	// f_fsid is two 32-bit words on every unix target x/sys supports; their
	// signedness differs, so convert through uint32 to keep the bit pattern.
	return uint64(uint32(stat.Fsid.Val[0]))<<32 | uint64(uint32(stat.Fsid.Val[1]))
}
