//go:build unix

package synctree

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"

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
//
// Token is a random value written as the extended attribute
// RootMarkAttribute on the folder when the sync state binds to it. Inode and
// fsid are proxies that churn on FUSE, SMB and FAT mounts and on macOS fsid
// renumbering, and they can repeat: rm -rf and mkdir of the same path may get
// the inode back. The token is the folder's own name for itself, so when the
// recorded identity has one it alone decides (D-001). Filesystems without
// user extended attributes leave it empty and fall back to inode and fsid.
type RootIdentity struct {
	Device       uint64
	Inode        uint64
	FileSystemID uint64
	Token        string
}

// RootMarkAttribute is the extended attribute that carries a sync folder's
// bind token.
const RootMarkAttribute = "user.onedrive-go.root"

// btrfsSubvolumeRootInode is the inode every btrfs subvolume root has. Two
// subvolumes of one filesystem share the fsid and that inode, so for such a
// folder the device (one anonymous device per subvolume) must match too.
const btrfsSubvolumeRootInode = 256

// SameRoot reports whether two root fingerprints name the same directory.
//
// The inode must always match. The filesystem is matched by fsid when both
// sides carry one, because the device number of the same disk can change
// between boots; otherwise by device number. A false mismatch is safe (the
// engine refuses to sync until the user rebinds), a false match is not, so
// nothing here is looser than the old (device, inode) rule except the one
// reshuffle case fsid exists to cover.
//
// a is the recorded (expected) identity and b the live one; the order
// matters. When a carries a token, b
// must carry the same token: a match is the same folder whatever its inode
// now is, and a missing or different token is a different folder.
func SameRoot(a RootIdentity, b RootIdentity) bool {
	if a.Token != "" {
		return a.Token == b.Token
	}
	if a.Inode != b.Inode {
		return false
	}
	if a.FileSystemID != 0 && b.FileSystemID != 0 {
		if a.FileSystemID != b.FileSystemID {
			return false
		}
		if a.Inode == btrfsSubvolumeRootInode {
			return a.Device == b.Device
		}
		return true
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
		Token:        r.readRootMark(dir),
	}, nil
}

// rootMarkMaxBytes bounds the attribute read; tokens are far shorter.
const rootMarkMaxBytes = 256

const rootMarkRandomBytes = 16

// readRootMark returns the folder's bind token, or "" when it has none or the
// filesystem cannot store one. A read failure reads as "no token", which only
// ever makes a recorded token mismatch: safe.
func (r *Root) readRootMark(dir *os.File) string {
	token, err := r.ops.getRootMark(dir)
	if err != nil {
		return ""
	}
	return token
}

// MarkRoot writes a fresh random bind token on the sync folder and returns it.
// ErrRootMarkUnsupported means the filesystem has no user extended
// attributes; the caller binds without a token.
func (r *Root) MarkRoot() (string, error) {
	var random [rootMarkRandomBytes]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("generating sync folder token: %w", err)
	}
	token := hex.EncodeToString(random[:])

	dir, err := r.Open(".")
	if err != nil {
		return "", err
	}
	defer dir.Close() //nolint:errcheck // read-only descriptor; nothing to flush

	if err := r.ops.setRootMark(dir, token); err != nil {
		if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EPERM) {
			return "", fmt.Errorf("%w: %w", ErrRootMarkUnsupported, err)
		}
		return "", fmt.Errorf("marking sync folder %s: %w", r.dir, err)
	}

	return token, nil
}

// ErrRootMarkUnsupported reports a filesystem that cannot store the sync
// folder's bind token.
var ErrRootMarkUnsupported = errors.New("this filesystem cannot store extended attributes")

func fileSystemIDFromStatfs(stat *unix.Statfs_t) uint64 {
	// f_fsid is two 32-bit words on every unix target x/sys supports; their
	// signedness differs, so convert through uint32 to keep the bit pattern.
	return uint64(uint32(stat.Fsid.Val[0]))<<32 | uint64(uint32(stat.Fsid.Val[1]))
}
