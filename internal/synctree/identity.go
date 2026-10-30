package synctree

import (
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// FileIdentity is a platform filesystem identity for a directory entry under a
// sync tree. It is useful for local rename detection only; it is not a content
// identity and must not be used to authorize cross-directory moves.
//
// (Device, Inode) alone does not name one entry over time: ext4, tmpfs and
// most Linux filesystems hand a just-freed inode number to the next entry
// created, so a deleted entry and an unrelated new one can share it. The
// entry's birth time tells them apart wherever the filesystem records one,
// because a rename keeps it and a new entry gets its own.
type FileIdentity struct {
	Device uint64
	Inode  uint64
	// BirthTime is the entry's creation time in Unix nanoseconds, or zero
	// when the filesystem or platform does not report one.
	BirthTime int64
}

// BirthTimeReporter is implemented by a FileInfo that states its entry's
// birth time itself: on Linux, where the stat the standard library returns
// carries none (statx supplies it, see withBirthTime), and in the filesystem
// shims of internal/synctest. Its answer is authoritative, including an
// unknown one.
type BirthTimeReporter interface {
	BirthTime() (nanos int64, known bool)
}

// birthTimeInfo is a FileInfo carrying the birth time statx reported.
type birthTimeInfo struct {
	fs.FileInfo
	birthTime int64
}

func (i birthTimeInfo) BirthTime() (int64, bool) {
	return i.birthTime, i.birthTime != 0
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
	if reporter, reports := info.(BirthTimeReporter); reports {
		if birthTime, known := reporter.BirthTime(); known {
			identity.BirthTime = birthTime
		}
	} else {
		identity.BirthTime = statBirthTime(stat)
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

// SameIdentity is the one definition of "the same filesystem entry": the
// same device and inode, and the same birth time whenever both sides know
// one. A side without a birth time (a filesystem that records none) falls
// back to (device, inode), which cannot tell inode reuse from a rename. The
// SQL comparison queries apply the same rule (the sqlSameLocalEntry* constants in
// internal/sync).
func SameIdentity(a FileIdentity, b FileIdentity) bool {
	return a.Device == b.Device && a.Inode == b.Inode &&
		(a.BirthTime == 0 || b.BirthTime == 0 || a.BirthTime == b.BirthTime)
}

// SameFile reports whether a and b describe the same filesystem entry. It
// replaces os.SameFile for FileInfo values the sync tree returns, which may
// be wrapped to carry a birth time and so are not what os.SameFile accepts.
func SameFile(a, b fs.FileInfo) bool {
	identityA, okA := IdentityFromFileInfo(a)
	identityB, okB := IdentityFromFileInfo(b)
	if okA && okB {
		return SameIdentity(identityA, identityB)
	}

	return os.SameFile(a, b)
}

func (r *Root) IdentityNoFollow(rel string) (FileIdentity, error) {
	info, err := r.Lstat(rel)
	if err != nil {
		// Lstat already names the path; restating it here only stuttered.
		return FileIdentity{}, err
	}
	identity, ok := IdentityFromFileInfo(info)
	if !ok {
		return FileIdentity{}, fmt.Errorf("can't identify folder %s: this filesystem reports no stable file identity", rel)
	}

	return identity, nil
}
