//go:build linux

package synctree

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

// The in-memory volume keeps names and folders in Go maps and the bytes of
// every entry in a memfd, an anonymous file the kernel holds in memory. A
// memfd is a real open file, so the handle can return the *os.File every
// caller of the sync tree expects, the kernel keeps size, mtime and ctime
// as a write changes them, and the memfd's inode number is a stable
// identity that a rename keeps. Folders have a memfd too, for that identity
// and for the descriptor RootIdentity reads. Nothing is ever written to a
// folder on disk.
//
// What it does not model: symbolic links and hard links (an entry has one
// name), a rename changing the entry's ctime, permissions refusing access,
// and anything outside the tree (detaching to a trash folder, moving a file
// in), which fails with ErrOutsideMemoryVolume.

// memoryRootParent is where in-memory roots claim to live. An unprivileged
// process cannot create it, so a caller that wrongly reaches the root by its
// path finds nothing instead of writing to the disk.
const memoryRootParent = "/nonexistent/onedrive-go-memory-volume"

// OpenMemory returns a Root over a new, empty in-memory volume that compares
// names as caps says. Every rooted operation runs on the volume; Path names
// a folder that does not exist on disk.
func OpenMemory(caps Capabilities) (*Root, error) {
	vol, err := newMemoryVolume(caps)
	if err != nil {
		return nil, err
	}

	// The root folder's memfd inode names the root: the kernel keeps it
	// unique among live memfds, so each open volume has its own path.
	rootInfo, err := vol.stat(".", "lstat")
	if err != nil {
		vol.root.release()
		return nil, err
	}
	rootStat, isStat := rootInfo.Sys().(*syscall.Stat_t)
	if !isStat {
		vol.root.release()
		return nil, fmt.Errorf("stating in-memory root: unexpected stat type %T", rootInfo.Sys())
	}
	dir := filepath.Join(memoryRootParent, strconv.FormatUint(rootStat.Ino, 10))
	ops := defaultRootOps()
	ops.openRoot = func(string) (rootHandle, error) { return &memoryHandle{vol: vol}, nil }
	ops.rmdir = func(_ rootHandle, name string) error { return vol.rmdir(name) }
	ops.lstatAbs = func(path string) (os.FileInfo, error) {
		if filepath.Clean(path) != dir {
			return nil, &os.PathError{Op: "lstat", Path: path, Err: syscall.ENOENT}
		}
		return vol.stat(".", "lstat")
	}
	ops.getRootMark = func(*os.File) (string, error) { return vol.rootMark(), nil }
	ops.setRootMark = func(_ *os.File, token string) error {
		vol.setRootMark(token)
		return nil
	}

	return &Root{dir: dir, ops: ops, memory: vol}, nil
}

type memoryVolume struct {
	caps Capabilities

	mu   sync.Mutex // guards root's tree and mark
	root *memoryNode
	mark string
}

type memoryNode struct {
	name     string
	dir      bool
	children map[string]*memoryNode
	// content is the memfd holding the entry's bytes and metadata; fd is
	// its descriptor, read once so the file stays in its blocking mode.
	content *os.File
	fd      int
}

func newMemoryVolume(caps Capabilities) (*memoryVolume, error) {
	vol := &memoryVolume{caps: caps}
	root, err := vol.newNode(".", true, localpath.UserTreeDirPerm)
	if err != nil {
		return nil, err
	}
	vol.root = root

	return vol, nil
}

func (v *memoryVolume) newNode(name string, dir bool, perm os.FileMode) (*memoryNode, error) {
	fd, err := unix.MemfdCreate("onedrive-go-memory-volume", unix.MFD_CLOEXEC)
	if err != nil {
		return nil, fmt.Errorf("creating in-memory entry %s: %w", name, err)
	}
	if err := unix.Fchmod(fd, uint32(perm.Perm())); err != nil {
		_ = unix.Close(fd) //nolint:errcheck // error path; the chmod failure is reported
		return nil, fmt.Errorf("setting mode of in-memory entry %s: %w", name, err)
	}
	node := &memoryNode{name: name, dir: dir, content: os.NewFile(uintptr(fd), name), fd: fd}
	if dir {
		node.children = make(map[string]*memoryNode)
	}

	return node, nil
}

func (n *memoryNode) release() {
	for _, child := range n.children {
		child.release()
	}
	_ = n.content.Close() //nolint:errcheck // the memfd lives on in any file still open on it
}

func pathError(op, name string, errno syscall.Errno) error {
	return &os.PathError{Op: op, Path: name, Err: errno}
}

// errnoOf returns the Errno a unix call failed with; any other error, which
// the unix package does not return, reads as EIO.
func errnoOf(err error) syscall.Errno {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno
	}

	return syscall.EIO
}

// parentName is the name that climbs out of a folder.
const parentName = ".."

// nameParts splits a rooted name into its parts; "." is the root itself.
func nameParts(name string) ([]string, error) {
	clean := filepath.Clean(filepath.FromSlash(name))
	if clean == "." {
		return nil, nil
	}
	if filepath.IsAbs(clean) || clean == parentName || strings.HasPrefix(clean, parentName+string(filepath.Separator)) {
		return nil, fmt.Errorf("path escapes from parent")
	}

	return strings.Split(clean, string(filepath.Separator)), nil
}

// lookup finds the entry of dir named part, the exact spelling first and
// then, on a volume that folds names, any equivalent one.
func (v *memoryVolume) lookup(dir *memoryNode, part string) *memoryNode {
	if child, ok := dir.children[part]; ok {
		return child
	}
	if !v.caps.folds() {
		return nil
	}
	want := v.caps.fold(part)
	names := make([]string, 0, len(dir.children))
	for stored := range dir.children {
		names = append(names, stored)
	}
	sort.Strings(names)
	for _, stored := range names {
		if v.caps.fold(stored) == want {
			return dir.children[stored]
		}
	}

	return nil
}

// resolve walks name and returns its parent folder, its base name and the
// entry, nil when the base is missing. A missing or non-folder ancestor is
// ENOENT or ENOTDIR. The root resolves to a nil parent.
func (v *memoryVolume) resolve(op, name string) (*memoryNode, string, *memoryNode, error) {
	parts, err := nameParts(name)
	if err != nil {
		return nil, "", nil, &os.PathError{Op: op, Path: name, Err: err}
	}
	if len(parts) == 0 {
		return nil, ".", v.root, nil
	}

	dir := v.root
	for _, part := range parts[:len(parts)-1] {
		child := v.lookup(dir, part)
		if child == nil {
			return nil, "", nil, pathError(op, name, syscall.ENOENT)
		}
		if !child.dir {
			return nil, "", nil, pathError(op, name, syscall.ENOTDIR)
		}
		dir = child
	}
	base := parts[len(parts)-1]

	return dir, base, v.lookup(dir, base), nil
}

func (v *memoryVolume) existing(op, name string) (*memoryNode, *memoryNode, error) {
	parent, _, node, err := v.resolve(op, name)
	if err != nil {
		return nil, nil, err
	}
	if node == nil {
		return nil, nil, pathError(op, name, syscall.ENOENT)
	}

	return parent, node, nil
}

func (v *memoryVolume) info(node *memoryNode) (os.FileInfo, error) {
	var st syscall.Stat_t
	if err := syscall.Fstat(node.fd, &st); err != nil {
		return nil, fmt.Errorf("stating in-memory entry %s: %w", node.name, err)
	}
	info := &memoryInfo{
		name:  filepath.Base(node.name),
		size:  st.Size,
		mode:  os.FileMode(st.Mode) & os.ModePerm,
		mtime: time.Unix(st.Mtim.Sec, st.Mtim.Nsec),
		sys:   st,
	}
	info.sys.Nlink = 1
	if node.dir {
		info.mode |= os.ModeDir
		info.size = 0
		info.sys.Mode = st.Mode&^syscall.S_IFMT | syscall.S_IFDIR
		info.sys.Nlink = 2
		info.sys.Size = 0
	}
	var stx unix.Statx_t
	if err := unix.Statx(node.fd, "", unix.AT_EMPTY_PATH, unix.STATX_BTIME, &stx); err == nil && stx.Mask&unix.STATX_BTIME != 0 {
		info.birth = time.Unix(stx.Btime.Sec, int64(stx.Btime.Nsec)).UnixNano()
	}

	return info, nil
}

func (v *memoryVolume) stat(name, op string) (os.FileInfo, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	_, node, err := v.existing(op, name)
	if err != nil {
		return nil, err
	}

	return v.info(node)
}

// reopen opens a new description of node's memfd, with its own offset, as
// an open of the entry's name would.
func reopen(node *memoryNode, name string, flag int) (*os.File, error) {
	keep := flag & (unix.O_ACCMODE | unix.O_TRUNC | unix.O_APPEND | unix.O_SYNC | unix.O_DSYNC)
	fd, err := unix.Open("/proc/self/fd/"+strconv.Itoa(node.fd), keep|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, pathError("open", name, errnoOf(err))
	}

	return os.NewFile(uintptr(fd), name), nil
}

func (v *memoryVolume) openFile(name string, flag int, perm os.FileMode) (*os.File, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	parent, base, node, err := v.resolve("open", name)
	if err != nil {
		return nil, err
	}
	writes := flag&(os.O_WRONLY|os.O_RDWR|os.O_TRUNC|os.O_APPEND) != 0
	switch {
	case node != nil && flag&os.O_CREATE != 0 && flag&os.O_EXCL != 0:
		return nil, pathError("open", name, syscall.EEXIST)
	case node != nil && node.dir && writes:
		return nil, pathError("open", name, syscall.EISDIR)
	case node != nil && !node.dir && flag&unix.O_DIRECTORY != 0:
		return nil, pathError("open", name, syscall.ENOTDIR)
	case node != nil:
		return reopen(node, name, flag)
	case flag&os.O_CREATE == 0:
		return nil, pathError("open", name, syscall.ENOENT)
	}

	created, err := v.newNode(base, false, perm)
	if err != nil {
		return nil, err
	}
	parent.children[base] = created

	return reopen(created, name, flag&^os.O_TRUNC)
}

func (v *memoryVolume) mkdir(name string, perm os.FileMode) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	parent, base, node, err := v.resolve("mkdir", name)
	if err != nil {
		return err
	}
	if node != nil {
		return pathError("mkdir", name, syscall.EEXIST)
	}
	created, err := v.newNode(base, true, perm)
	if err != nil {
		return err
	}
	parent.children[base] = created

	return nil
}

func (v *memoryVolume) mkdirAll(name string, perm os.FileMode) error {
	parts, err := nameParts(name)
	if err != nil {
		return &os.PathError{Op: "mkdir", Path: name, Err: err}
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	dir := v.root
	for _, part := range parts {
		child := v.lookup(dir, part)
		if child == nil {
			created, createErr := v.newNode(part, true, perm)
			if createErr != nil {
				return createErr
			}
			dir.children[part] = created
			child = created
		}
		if !child.dir {
			return pathError("mkdir", name, syscall.ENOTDIR)
		}
		dir = child
	}

	return nil
}

// remove takes name off the tree as remove(3) does: a file, or a folder only
// when it is empty.
func (v *memoryVolume) remove(name string, folderOnly bool) error {
	op := "remove"
	if folderOnly {
		op = "rmdir"
	}
	v.mu.Lock()
	defer v.mu.Unlock()

	parent, node, err := v.existing(op, name)
	if err != nil {
		return err
	}
	switch {
	case parent == nil:
		return pathError(op, name, syscall.EBUSY)
	case folderOnly && !node.dir:
		return pathError(op, name, syscall.ENOTDIR)
	case node.dir && len(node.children) > 0:
		return pathError(op, name, syscall.ENOTEMPTY)
	}
	delete(parent.children, node.name)
	node.release()

	return nil
}

func (v *memoryVolume) rmdir(name string) error { return v.remove(name, true) }

func (v *memoryVolume) removeAll(name string) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	parent, _, node, err := v.resolve("unlinkat", name)
	if err != nil {
		var pathErr *os.PathError
		if errors.As(err, &pathErr) && pathErr.Err == syscall.ENOENT { //nolint:errorlint // pathError stores a bare Errno
			return nil
		}
		return err
	}
	if node == nil {
		return nil
	}
	if parent == nil {
		return pathError("unlinkat", name, syscall.EINVAL)
	}
	delete(parent.children, node.name)
	node.release()

	return nil
}

func within(ancestor, node *memoryNode) bool {
	if ancestor == node {
		return true
	}
	for _, child := range ancestor.children {
		if child.dir && within(child, node) {
			return true
		}
	}

	return false
}

// rename moves oldname to newname as rename(2) does; with noReplace an entry
// at newname other than the source itself fails with EEXIST. On a volume
// that folds names, a newname equivalent to the source's own name respells
// it.
func (v *memoryVolume) rename(oldname, newname string, noReplace bool) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	srcParent, src, err := v.existing("rename", oldname)
	if err != nil {
		return err
	}
	dstParent, dstBase, dst, err := v.resolve("rename", newname)
	if err != nil {
		return err
	}
	if srcParent == nil || dstParent == nil {
		return pathError("rename", oldname, syscall.EBUSY)
	}
	if src.dir && within(src, dstParent) {
		return pathError("rename", oldname, syscall.EINVAL)
	}

	if dst != nil && dst != src {
		if errno, refused := replaceRefusal(src, dst, noReplace); refused {
			return pathError("rename", newname, errno)
		}
		delete(dstParent.children, dst.name)
		dst.release()
	}

	delete(srcParent.children, src.name)
	src.name = dstBase
	dstParent.children[dstBase] = src

	return nil
}

// replaceRefusal says why rename(2) would refuse to replace dst with src,
// if it would.
func replaceRefusal(src, dst *memoryNode, noReplace bool) (syscall.Errno, bool) {
	switch {
	case noReplace:
		return syscall.EEXIST, true
	case src.dir && !dst.dir:
		return syscall.ENOTDIR, true
	case !src.dir && dst.dir:
		return syscall.EISDIR, true
	case dst.dir && len(dst.children) > 0:
		return syscall.ENOTEMPTY, true
	default:
		return 0, false
	}
}

func (v *memoryVolume) chtimes(name string, atime, mtime time.Time) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	_, node, err := v.existing("chtimes", name)
	if err != nil {
		return err
	}
	times := []unix.Timespec{timespecOrOmit(atime), timespecOrOmit(mtime)}
	if err := unix.UtimesNanoAt(unix.AT_FDCWD, "/proc/self/fd/"+strconv.Itoa(node.fd), times, 0); err != nil {
		return pathError("chtimes", name, errnoOf(err))
	}

	return nil
}

// timespecOrOmit leaves a zero time unchanged, as os.Chtimes does.
func timespecOrOmit(t time.Time) unix.Timespec {
	if t.IsZero() {
		return unix.Timespec{Nsec: unix.UTIME_OMIT}
	}

	return unix.NsecToTimespec(t.UnixNano())
}

func (v *memoryVolume) rootMark() string {
	v.mu.Lock()
	defer v.mu.Unlock()

	return v.mark
}

func (v *memoryVolume) setRootMark(token string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.mark = token
}

// removeVerified removes the regular file at name if it is still the entry
// verify approved, unchanged since: RemoveVerifiedRegularFile's binding,
// held here by the volume's lock instead of a set-aside rename.
func (v *memoryVolume) removeVerified(name, displayName string, verify VerifyFunc) error {
	v.mu.Lock()
	_, node, err := v.existing("open", name)
	if err != nil {
		v.mu.Unlock()
		return err
	}
	if node.dir {
		v.mu.Unlock()
		return unsupportedEntryf("%s is not a regular file", displayName)
	}
	file, err := reopen(node, name, os.O_RDONLY)
	v.mu.Unlock()
	if err != nil {
		return err
	}
	defer file.Close() //nolint:errcheck // read-only descriptor held for verification

	checked := &verifiedFile{file: file}
	if checkErr := checked.check(displayName, verify); checkErr != nil {
		return checkErr
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	parent, current, err := v.existing("remove", name)
	if err != nil {
		return fmt.Errorf("removing %s: %w", displayName, ErrDetachRaced)
	}
	if current != node {
		return fmt.Errorf("removing %s: %w", displayName, ErrDetachRaced)
	}
	if err := unchangedBeforeMove(file, checked.stamp); err != nil {
		return fmt.Errorf("removing %s: %w", displayName, err)
	}
	delete(parent.children, node.name)
	node.release()

	return nil
}

// memoryHandle is the Handle over a volume. Closing it releases nothing:
// the volume outlives every handle, as a disk outlives a descriptor.
type memoryHandle struct {
	vol *memoryVolume
}

func (h *memoryHandle) Open(name string) (*os.File, error) {
	return h.vol.openFile(name, os.O_RDONLY, 0)
}

func (h *memoryHandle) OpenFile(name string, flag int, perm os.FileMode) (*os.File, error) {
	return h.vol.openFile(name, flag, perm)
}

func (h *memoryHandle) Stat(name string) (os.FileInfo, error) { return h.vol.stat(name, "stat") }

func (h *memoryHandle) Lstat(name string) (os.FileInfo, error) { return h.vol.stat(name, "lstat") }

func (h *memoryHandle) Mkdir(name string, perm os.FileMode) error { return h.vol.mkdir(name, perm) }

func (h *memoryHandle) MkdirAll(name string, perm os.FileMode) error {
	return h.vol.mkdirAll(name, perm)
}

func (h *memoryHandle) Remove(name string) error { return h.vol.remove(name, false) }

func (h *memoryHandle) RemoveAll(name string) error { return h.vol.removeAll(name) }

func (h *memoryHandle) Rename(oldname, newname string) error {
	return h.vol.rename(oldname, newname, false)
}

func (h *memoryHandle) Chtimes(name string, atime, mtime time.Time) error {
	return h.vol.chtimes(name, atime, mtime)
}

func (h *memoryHandle) FS() fs.FS { return memoryFS{vol: h.vol} }

func (h *memoryHandle) Close() error { return nil }

// memoryInfo is the FileInfo of an in-memory entry: the memfd's stat with
// the entry's name and kind, carrying its birth time.
type memoryInfo struct {
	name  string
	size  int64
	mode  os.FileMode
	mtime time.Time
	sys   syscall.Stat_t
	birth int64
}

func (i *memoryInfo) Name() string       { return i.name }
func (i *memoryInfo) Size() int64        { return i.size }
func (i *memoryInfo) Mode() os.FileMode  { return i.mode }
func (i *memoryInfo) ModTime() time.Time { return i.mtime }
func (i *memoryInfo) IsDir() bool        { return i.mode.IsDir() }
func (i *memoryInfo) Sys() any           { return &i.sys }

func (i *memoryInfo) BirthTime() (int64, bool) { return i.birth, i.birth != 0 }

// memoryFS is the read side of the volume for walks and listings.
type memoryFS struct {
	vol *memoryVolume
}

func (f memoryFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}

	f.vol.mu.Lock()
	defer f.vol.mu.Unlock()
	_, node, err := f.vol.existing("open", name)
	if err != nil {
		return nil, err
	}
	info, err := f.vol.info(node)
	if err != nil {
		return nil, err
	}
	if !node.dir {
		file, openErr := reopen(node, name, os.O_RDONLY)
		if openErr != nil {
			return nil, openErr
		}
		return &memoryFSFile{File: file, info: info}, nil
	}

	names := make([]string, 0, len(node.children))
	for stored := range node.children {
		names = append(names, stored)
	}
	sort.Strings(names)
	entries := make([]fs.DirEntry, 0, len(names))
	for _, stored := range names {
		childInfo, infoErr := f.vol.info(node.children[stored])
		if infoErr != nil {
			return nil, infoErr
		}
		entries = append(entries, fs.FileInfoToDirEntry(childInfo))
	}

	return &memoryFSDir{info: info, entries: entries}, nil
}

type memoryFSFile struct {
	*os.File
	info os.FileInfo
}

func (f *memoryFSFile) Stat() (fs.FileInfo, error) { return f.info, nil }

type memoryFSDir struct {
	info    os.FileInfo
	entries []fs.DirEntry
	offset  int
}

func (d *memoryFSDir) Stat() (fs.FileInfo, error) { return d.info, nil }

func (d *memoryFSDir) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.info.Name(), Err: syscall.EISDIR}
}

func (d *memoryFSDir) Close() error { return nil }

func (d *memoryFSDir) ReadDir(n int) ([]fs.DirEntry, error) {
	rest := d.entries[d.offset:]
	if n <= 0 {
		d.offset = len(d.entries)
		return rest, nil
	}
	if len(rest) == 0 {
		return nil, io.EOF
	}
	if n > len(rest) {
		n = len(rest)
	}
	d.offset += n

	return rest[:n], nil
}
