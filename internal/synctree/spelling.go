package synctree

import (
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/text/unicode/norm"
)

// Spelling returns rel as the filesystem spells it: each component that
// names an existing entry is replaced by that entry's own name when the two
// differ only in Unicode normalization (R-2.13.2).
//
// Sync stores every path in NFC (R-2.13.1). That is enough on macOS, whose
// volumes find an entry by either spelling, but a byte-exact filesystem
// (Linux ext4, Btrfs, XFS) answers ENOENT for the NFC spelling of an entry
// whose name on disk is NFD, so every operation on an existing entry has to
// address it by the bytes on disk.
//
// The exact path is tried first, so a path that exists as spelled costs one
// lstat. Only when it is missing is the path walked from the sync root: a
// component that exists as spelled is kept, and a missing one is looked up in
// its parent's listing for the single entry with the same NFC form. A
// component nothing matches ends the walk, and it and everything below it
// keep the caller's spelling, so a path to an entry not yet created comes
// back with its existing parents resolved and its new names as given. Two or
// more entries with the same NFC form are a local name collision (R-2.12.3)
// that never syncs; the lookup does not pick one and treats the component as
// missing.
//
// Spelling never fails: a path it cannot resolve (unsafe, unreadable parent,
// sync folder unavailable) comes back as given, and the operation that
// follows reports why it cannot be used.
func (r *Root) Spelling(rel string) string {
	clean, err := cleanRelative(rel)
	if err != nil || clean == "." {
		return rel
	}

	root, err := r.ops.openRoot(r.dir)
	if err != nil {
		return rel
	}
	defer func() {
		// Read-only use of the handle: a close failure loses nothing.
		_ = root.Close() //nolint:errcheck // see above
	}()

	if _, statErr := root.Lstat(clean); !isMissing(statErr) {
		return clean
	}

	return spellingWalk(root, clean)
}

// isMissing reports an lstat answer that means the name, as spelled, is not
// there. Any other answer (present, or an error such as EACCES) leaves the
// spelling alone.
func isMissing(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

func spellingWalk(root rootHandle, clean string) string {
	components := strings.Split(clean, string(filepath.Separator))
	resolved := ""
	for i, component := range components {
		candidate := filepath.Join(resolved, component)
		_, statErr := root.Lstat(candidate)
		if statErr == nil {
			resolved = candidate
			continue
		}
		if !isMissing(statErr) {
			return filepath.Join(append([]string{resolved}, components[i:]...)...)
		}

		name, found := uniqueEquivalentEntry(root, resolved, component)
		if !found {
			return filepath.Join(append([]string{resolved}, components[i:]...)...)
		}
		resolved = filepath.Join(resolved, name)
	}

	return resolved
}

// uniqueEquivalentEntry finds the one entry of dir (rooted, "" for the sync
// root) whose name has component's NFC form.
func uniqueEquivalentEntry(root rootHandle, dir string, component string) (string, bool) {
	if dir == "" {
		dir = "."
	}
	entries, err := fs.ReadDir(root.FS(), filepath.ToSlash(dir))
	if err != nil {
		return "", false
	}

	want := norm.NFC.String(component)
	match := ""
	for _, entry := range entries {
		if norm.NFC.String(entry.Name()) != want {
			continue
		}
		if match != "" {
			return "", false
		}
		match = entry.Name()
	}

	return match, match != ""
}

// SpellingOfParent returns rel with its parent folders spelled as on disk
// and its final name as given: the spelling of an entry about to be created.
// It never lists the final component's folder, so creating many entries in
// one large folder does not read that folder once per entry.
func (r *Root) SpellingOfParent(rel string) string {
	clean, err := cleanRelative(rel)
	if err != nil || clean == "." {
		return rel
	}
	dir := filepath.Dir(clean)
	if dir == "." {
		return clean
	}

	return filepath.Join(r.Spelling(dir), filepath.Base(clean))
}
