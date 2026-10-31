package synctest

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// IdentityRule makes the entry at Path report a chosen filesystem identity.
// The kernel cannot be made to reuse an inode, but a rule can report a new
// entry with a deleted entry's inode, which is what ext4 or tmpfs may do
// after a delete and a create (D-094).
type IdentityRule struct {
	// Path is the exact sync-root-relative slash path the rule applies to.
	Path string
	// Inode replaces the entry's inode number; zero keeps the real one.
	Inode uint64
	// BirthTime is the creation time the entry reports, in Unix
	// nanoseconds; zero reports a filesystem that records none.
	BirthTime int64
}

// IdentityShim reports the identities its rules give, through every stat
// and directory listing of the sync tree. Paths without a rule report the
// real filesystem's identity. Rules can change between passes.
type IdentityShim struct {
	mu    sync.Mutex // guards rules
	rules map[string]IdentityRule
}

// NewIdentityShim returns a shim applying rules.
func NewIdentityShim(rules ...IdentityRule) *IdentityShim {
	shim := &IdentityShim{rules: make(map[string]IdentityRule)}
	for _, rule := range rules {
		shim.rules[rule.Path] = rule
	}

	return shim
}

// Set adds or replaces the rule for rule.Path.
func (s *IdentityShim) Set(rule IdentityRule) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rules[rule.Path] = rule
}

// Wrap is the synctree.Root.Interpose function installing the shim.
func (s *IdentityShim) Wrap(h synctree.Handle) synctree.Handle {
	return &identityHandle{Handle: h, shim: s}
}

func (s *IdentityShim) rule(name string) (IdentityRule, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rule, ok := s.rules[path.Clean(filepath.ToSlash(name))]

	return rule, ok
}

// report applies the rule for name, if any, to info.
func (s *IdentityShim) report(name string, info fs.FileInfo) fs.FileInfo {
	rule, ok := s.rule(name)
	if !ok || info == nil {
		return info
	}

	return ruledInfo{FileInfo: info, rule: rule}
}

// ruledInfo is a FileInfo reporting a rule's identity: its Sys() is a copy
// of the real stat with the rule's inode, and it states the rule's birth
// time as synctree.BirthTimeReporter.
type ruledInfo struct {
	fs.FileInfo
	rule IdentityRule
}

func (i ruledInfo) Sys() any {
	stat, ok := i.FileInfo.Sys().(*syscall.Stat_t)
	if !ok || stat == nil || i.rule.Inode == 0 {
		return i.FileInfo.Sys()
	}
	reported := *stat
	reported.Ino = i.rule.Inode

	return &reported
}

func (i ruledInfo) BirthTime() (int64, bool) {
	return i.rule.BirthTime, i.rule.BirthTime != 0
}

type identityHandle struct {
	synctree.Handle
	shim *IdentityShim
}

func (h *identityHandle) Stat(name string) (os.FileInfo, error) {
	info, err := h.Handle.Stat(name)
	if err != nil {
		return nil, err //nolint:wrapcheck // the shim stands in for the filesystem
	}

	return h.shim.report(name, info), nil
}

func (h *identityHandle) Lstat(name string) (os.FileInfo, error) {
	info, err := h.Handle.Lstat(name)
	if err != nil {
		return nil, err //nolint:wrapcheck // the shim stands in for the filesystem
	}

	return h.shim.report(name, info), nil
}

func (h *identityHandle) FS() fs.FS {
	return identityFS{fsys: h.Handle.FS(), shim: h.shim}
}

// identityFS is the read side of the shim: directory walks and listings see
// the ruled identities through the entries' Info and the files' Stat.
type identityFS struct {
	fsys fs.FS
	shim *IdentityShim
}

func (f identityFS) Open(name string) (fs.File, error) {
	file, err := f.fsys.Open(name)
	if err != nil {
		return nil, err //nolint:wrapcheck // the shim stands in for the filesystem
	}

	return &identityFile{File: file, name: name, shim: f.shim}, nil
}

type identityFile struct {
	fs.File
	name string
	shim *IdentityShim
}

func (f *identityFile) Stat() (fs.FileInfo, error) {
	info, err := f.File.Stat()
	if err != nil {
		return nil, err //nolint:wrapcheck // the shim stands in for the filesystem
	}

	return f.shim.report(f.name, info), nil
}

var errNotADirectoryFile = errors.New("not a directory file")

func (f *identityFile) ReadDir(n int) ([]fs.DirEntry, error) {
	dir, ok := f.File.(fs.ReadDirFile)
	if !ok {
		return nil, &fs.PathError{Op: "readdir", Path: f.name, Err: errNotADirectoryFile}
	}
	entries, err := dir.ReadDir(n)
	for i, entry := range entries {
		entries[i] = identityDirEntry{DirEntry: entry, name: path.Join(f.name, entry.Name()), shim: f.shim}
	}

	return entries, err //nolint:wrapcheck // the shim stands in for the filesystem
}

type identityDirEntry struct {
	fs.DirEntry
	name string
	shim *IdentityShim
}

func (e identityDirEntry) Info() (fs.FileInfo, error) {
	info, err := e.DirEntry.Info()
	if err != nil {
		return nil, err //nolint:wrapcheck // the shim stands in for the filesystem
	}

	return e.shim.report(e.name, info), nil
}
