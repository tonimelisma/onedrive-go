package synctest

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"

	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// Filesystem shims (D-094). Each wraps the sync tree's contained handle
// (synctree.Handle) and is installed with synctree.Root.Interpose, so every
// rooted operation the sync engine performs meets the emulated filesystem.
// Reads the engine makes outside the sync tree (upload sources and content
// hashing through internal/localpath) see the real filesystem underneath.

// FSOp names the operations a shim rule matches.
type FSOp string

const (
	// FSOpAny matches every operation.
	FSOpAny FSOp = ""
	// FSOpOpen is a read-only open, including directory reads.
	FSOpOpen FSOp = "open"
	// FSOpWrite is an open for writing or creating.
	FSOpWrite FSOp = "write"
	// FSOpStat is Stat and Lstat.
	FSOpStat FSOp = "stat"
	// FSOpMkdir is Mkdir and MkdirAll.
	FSOpMkdir FSOp = "mkdir"
	// FSOpRemove is Remove and RemoveAll.
	FSOpRemove FSOp = "remove"
	// FSOpRename matches a rename by either of its names.
	FSOpRename FSOp = "rename"
	// FSOpChtimes is Chtimes.
	FSOpChtimes FSOp = "chtimes"
)

// ErrnoRule fails matching operations with Errno.
type ErrnoRule struct {
	Op FSOp
	// Path is a sync-root-relative slash path; the rule matches it and
	// everything beneath it. Empty matches every path.
	Path  string
	Errno syscall.Errno
}

func (r ErrnoRule) matches(op FSOp, name string) bool {
	if r.Op != FSOpAny && r.Op != op {
		return false
	}
	if r.Path == "" {
		return true
	}
	name = filepath.ToSlash(name)

	return name == r.Path || strings.HasPrefix(name, r.Path+"/")
}

// ErrnoShim fails sync-tree operations with injected errno values, as a
// full disk (ENOSPC), a read-only mount (EROFS), a permission change
// (EACCES), a failing disk (EIO) or a name the filesystem cannot hold
// (ENAMETOOLONG) would. Rules can change between passes.
type ErrnoShim struct {
	mu    sync.Mutex // guards rules and hits
	rules []ErrnoRule
	hits  int
}

// NewErrnoShim returns a shim injecting rules.
func NewErrnoShim(rules ...ErrnoRule) *ErrnoShim {
	return &ErrnoShim{rules: rules}
}

// SetRules replaces the rules; none heals the filesystem.
func (s *ErrnoShim) SetRules(rules ...ErrnoRule) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rules = rules
}

// Hits is how many operations the shim has failed.
func (s *ErrnoShim) Hits() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.hits
}

// Wrap is the synctree.Root.Interpose function installing the shim.
func (s *ErrnoShim) Wrap(h synctree.Handle) synctree.Handle {
	return &errnoHandle{Handle: h, shim: s}
}

func (s *ErrnoShim) fail(op FSOp, opName string, names ...string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rule := range s.rules {
		for _, name := range names {
			if rule.matches(op, name) {
				s.hits++
				return &os.PathError{Op: opName, Path: name, Err: rule.Errno}
			}
		}
	}

	return nil
}

type errnoHandle struct {
	synctree.Handle
	shim *ErrnoShim
}

func openOp(flag int) FSOp {
	if flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC|os.O_APPEND) != 0 {
		return FSOpWrite
	}

	return FSOpOpen
}

func (h *errnoHandle) Open(name string) (*os.File, error) {
	if err := h.shim.fail(FSOpOpen, "open", name); err != nil {
		return nil, err
	}

	return h.Handle.Open(name) //nolint:wrapcheck // the shim stands in for the filesystem
}

func (h *errnoHandle) OpenFile(name string, flag int, perm os.FileMode) (*os.File, error) {
	if err := h.shim.fail(openOp(flag), "open", name); err != nil {
		return nil, err
	}

	return h.Handle.OpenFile(name, flag, perm) //nolint:wrapcheck // the shim stands in for the filesystem
}

func (h *errnoHandle) Stat(name string) (os.FileInfo, error) {
	if err := h.shim.fail(FSOpStat, "stat", name); err != nil {
		return nil, err
	}

	return h.Handle.Stat(name) //nolint:wrapcheck // the shim stands in for the filesystem
}

func (h *errnoHandle) Lstat(name string) (os.FileInfo, error) {
	if err := h.shim.fail(FSOpStat, "lstat", name); err != nil {
		return nil, err
	}

	return h.Handle.Lstat(name) //nolint:wrapcheck // the shim stands in for the filesystem
}

func (h *errnoHandle) Mkdir(name string, perm os.FileMode) error {
	if err := h.shim.fail(FSOpMkdir, "mkdir", name); err != nil {
		return err
	}

	return h.Handle.Mkdir(name, perm) //nolint:wrapcheck // the shim stands in for the filesystem
}

func (h *errnoHandle) MkdirAll(name string, perm os.FileMode) error {
	if err := h.shim.fail(FSOpMkdir, "mkdir", name); err != nil {
		return err
	}

	return h.Handle.MkdirAll(name, perm) //nolint:wrapcheck // the shim stands in for the filesystem
}

func (h *errnoHandle) Remove(name string) error {
	if err := h.shim.fail(FSOpRemove, "remove", name); err != nil {
		return err
	}

	return h.Handle.Remove(name) //nolint:wrapcheck // the shim stands in for the filesystem
}

func (h *errnoHandle) RemoveAll(name string) error {
	if err := h.shim.fail(FSOpRemove, "remove", name); err != nil {
		return err
	}

	return h.Handle.RemoveAll(name) //nolint:wrapcheck // the shim stands in for the filesystem
}

func (h *errnoHandle) Rename(oldname, newname string) error {
	if err := h.shim.fail(FSOpRename, "rename", oldname, newname); err != nil {
		return err
	}

	return h.Handle.Rename(oldname, newname) //nolint:wrapcheck // the shim stands in for the filesystem
}

func (h *errnoHandle) Chtimes(name string, atime, mtime time.Time) error {
	if err := h.shim.fail(FSOpChtimes, "chtimes", name); err != nil {
		return err
	}

	return h.Handle.Chtimes(name, atime, mtime) //nolint:wrapcheck // the shim stands in for the filesystem
}

func (h *errnoHandle) FS() fs.FS {
	return errnoFS{fsys: h.Handle.FS(), shim: h.shim}
}

// errnoFS is the read side of the shim, which directory walks and listings
// use. It offers only Open, so fs.ReadDir and fs.Stat go through it.
type errnoFS struct {
	fsys fs.FS
	shim *ErrnoShim
}

func (f errnoFS) Open(name string) (fs.File, error) {
	if err := f.shim.fail(FSOpOpen, "open", name); err != nil {
		return nil, err
	}

	return f.fsys.Open(name) //nolint:wrapcheck // the shim stands in for the filesystem
}

// CaseInsensitive emulates a case-insensitive, case-preserving volume (the
// macOS default) over the case-sensitive one beneath: a name finds an
// existing entry that differs only in case or Unicode normalization, so a
// case variant of an existing file is that file (the same inode, as a hard
// link to it would be), creating a variant opens the existing entry, and a
// rename to a variant of the source's own name changes its spelling. Entries
// keep the spelling they were created with.
func CaseInsensitive(h synctree.Handle) synctree.Handle {
	return &caseInsensitiveHandle{Handle: h}
}

type caseInsensitiveHandle struct {
	synctree.Handle
}

// caseFold is the equivalence the emulated volume uses: case folding over
// canonical decomposition, as APFS compares names.
func caseFold(name string) string {
	return norm.NFD.String(cases.Fold().String(name))
}

// resolve maps a slash or OS name onto the spelling stored on disk,
// component by component; components that exist under no spelling keep the
// spelling asked for.
func (h *caseInsensitiveHandle) resolve(name string) string {
	clean := path.Clean(filepath.ToSlash(name))
	if clean == "." || clean == "" {
		return name
	}

	resolved := "."
	parts := strings.Split(clean, "/")
	for i, part := range parts {
		stored, found := h.lookup(resolved, part)
		if !found {
			return filepath.FromSlash(path.Join(append([]string{resolved}, parts[i:]...)...))
		}
		resolved = path.Join(resolved, stored)
	}

	return filepath.FromSlash(resolved)
}

// lookup finds the entry of dir named part under the emulated equivalence,
// preferring the exact spelling.
func (h *caseInsensitiveHandle) lookup(dir, part string) (string, bool) {
	entries, err := fs.ReadDir(h.Handle.FS(), dir)
	if err != nil {
		return "", false
	}
	key := caseFold(part)
	match := ""
	for _, entry := range entries {
		if entry.Name() == part {
			return part, true
		}
		if match == "" && caseFold(entry.Name()) == key {
			match = entry.Name()
		}
	}

	return match, match != ""
}

func (h *caseInsensitiveHandle) Open(name string) (*os.File, error) {
	return h.Handle.Open(h.resolve(name)) //nolint:wrapcheck // the shim stands in for the filesystem
}

func (h *caseInsensitiveHandle) OpenFile(name string, flag int, perm os.FileMode) (*os.File, error) {
	return h.Handle.OpenFile(h.resolve(name), flag, perm) //nolint:wrapcheck // the shim stands in for the filesystem
}

func (h *caseInsensitiveHandle) Stat(name string) (os.FileInfo, error) {
	return h.Handle.Stat(h.resolve(name)) //nolint:wrapcheck // the shim stands in for the filesystem
}

func (h *caseInsensitiveHandle) Lstat(name string) (os.FileInfo, error) {
	return h.Handle.Lstat(h.resolve(name)) //nolint:wrapcheck // the shim stands in for the filesystem
}

func (h *caseInsensitiveHandle) Mkdir(name string, perm os.FileMode) error {
	return h.Handle.Mkdir(h.resolve(name), perm) //nolint:wrapcheck // the shim stands in for the filesystem
}

func (h *caseInsensitiveHandle) MkdirAll(name string, perm os.FileMode) error {
	return h.Handle.MkdirAll(h.resolve(name), perm) //nolint:wrapcheck // the shim stands in for the filesystem
}

func (h *caseInsensitiveHandle) Remove(name string) error {
	return h.Handle.Remove(h.resolve(name)) //nolint:wrapcheck // the shim stands in for the filesystem
}

func (h *caseInsensitiveHandle) RemoveAll(name string) error {
	return h.Handle.RemoveAll(h.resolve(name)) //nolint:wrapcheck // the shim stands in for the filesystem
}

func (h *caseInsensitiveHandle) Chtimes(name string, atime, mtime time.Time) error {
	return h.Handle.Chtimes(h.resolve(name), atime, mtime) //nolint:wrapcheck // the shim stands in for the filesystem
}

// Rename follows a case-insensitive volume: renaming onto a variant of the
// source's own name respells it, and renaming onto a variant of another
// entry replaces that entry under the new spelling.
func (h *caseInsensitiveHandle) Rename(oldname, newname string) error {
	from := h.resolve(oldname)
	dir := h.resolve(filepath.Dir(newname))
	want := filepath.Join(dir, filepath.Base(newname))
	occupant := h.resolve(newname)

	if occupant != want {
		// A variant of the new name exists under another spelling.
		if occupant == from {
			return h.respell(from, want)
		}
		if err := h.Handle.Rename(from, occupant); err != nil {
			return err //nolint:wrapcheck // the shim stands in for the filesystem
		}

		return h.respell(occupant, want)
	}

	return h.Handle.Rename(from, want) //nolint:wrapcheck // the shim stands in for the filesystem
}

// respell changes an entry's stored spelling, through a temporary name as a
// case-sensitive filesystem needs.
func (h *caseInsensitiveHandle) respell(from, to string) error {
	if from == to {
		return nil
	}
	temp := filepath.Join(filepath.Dir(from), ".case-respell-"+strconv.Itoa(len(from))+"-"+filepath.Base(from))
	if err := h.Handle.Rename(from, temp); err != nil {
		return err //nolint:wrapcheck // the shim stands in for the filesystem
	}
	if err := h.Handle.Rename(temp, to); err != nil {
		return errors.Join(err, h.Handle.Rename(temp, from))
	}

	return nil
}

func (h *caseInsensitiveHandle) FS() fs.FS {
	return caseInsensitiveFS{h: h}
}

// caseInsensitiveFS resolves names for directory walks and listings; the
// entries it lists keep their stored spelling.
type caseInsensitiveFS struct {
	h *caseInsensitiveHandle
}

func (f caseInsensitiveFS) Open(name string) (fs.File, error) {
	return f.h.Handle.FS().Open(filepath.ToSlash(f.h.resolve(name))) //nolint:wrapcheck // the shim stands in for the filesystem
}
