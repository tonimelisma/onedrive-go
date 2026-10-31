package synctree

import (
	"errors"
	"io/fs"
	"path"
	"path/filepath"
)

// AttachBirthTime returns info, the stat of the entry at absPath, carrying
// the entry's birth time where the platform reads it separately (Linux,
// withBirthTime). Watch-mode observation stats entries by absolute path
// rather than through a Root, and uses it so a watch event records the same
// identity a full scan does.
func AttachBirthTime(info fs.FileInfo, absPath string) fs.FileInfo {
	if info == nil {
		return nil
	}

	return withBirthTime(info, absPath)
}

// birthTimeFS is the read side of the rooted handle: the FileInfo of every
// directory entry a walk or listing reads, and of every opened file, carries
// the entry's birth time like a rooted Lstat. It decorates lazily, so a
// listing that never asks an entry for its Info pays nothing.
type birthTimeFS struct {
	fsys fs.FS
	dir  string
}

func (f birthTimeFS) abs(name string) string {
	return filepath.Join(f.dir, filepath.FromSlash(name))
}

func (f birthTimeFS) Open(name string) (fs.File, error) {
	file, err := f.fsys.Open(name)
	if err != nil {
		return nil, err //nolint:wrapcheck // callers add the rooted path context
	}

	return &birthTimeFile{File: file, name: name, fsys: f}, nil
}

func (f birthTimeFS) ReadDir(name string) ([]fs.DirEntry, error) {
	entries, err := fs.ReadDir(f.fsys, name)
	f.wrapEntries(name, entries)

	return entries, err //nolint:wrapcheck // callers add the rooted path context
}

func (f birthTimeFS) Stat(name string) (fs.FileInfo, error) {
	info, err := fs.Stat(f.fsys, name)
	if err != nil {
		return nil, err //nolint:wrapcheck // callers add the rooted path context
	}

	return withBirthTime(info, f.abs(name)), nil
}

func (f birthTimeFS) wrapEntries(dir string, entries []fs.DirEntry) {
	for i, entry := range entries {
		entries[i] = birthTimeDirEntry{DirEntry: entry, abs: f.abs(path.Join(dir, entry.Name()))}
	}
}

type birthTimeFile struct {
	fs.File
	name string
	fsys birthTimeFS
}

func (f *birthTimeFile) Stat() (fs.FileInfo, error) {
	info, err := f.File.Stat()
	if err != nil {
		return nil, err //nolint:wrapcheck // callers add the rooted path context
	}

	return withBirthTime(info, f.fsys.abs(f.name)), nil
}

var errNotADirectory = errors.New("not a directory")

func (f *birthTimeFile) ReadDir(n int) ([]fs.DirEntry, error) {
	dir, ok := f.File.(fs.ReadDirFile)
	if !ok {
		return nil, &fs.PathError{Op: "readdir", Path: f.name, Err: errNotADirectory}
	}
	entries, err := dir.ReadDir(n)
	f.fsys.wrapEntries(f.name, entries)

	return entries, err //nolint:wrapcheck // callers add the rooted path context
}

type birthTimeDirEntry struct {
	fs.DirEntry
	abs string
}

func (e birthTimeDirEntry) Info() (fs.FileInfo, error) {
	info, err := e.DirEntry.Info()
	if err != nil {
		return nil, err //nolint:wrapcheck // callers add the rooted path context
	}

	return withBirthTime(info, e.abs), nil
}
