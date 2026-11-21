package syncobserve

import (
	"os"
	"path/filepath"
	"slices"

	"github.com/tonimelisma/onedrive-go/internal/namekey"
)

// DirNames is the set of names one directory holds on disk, grouped by the
// name OneDrive gives them (namekey.Of). It is the one collision rule of
// local observation: the full scan and the watch handlers both ask it, and
// both count only rivals that are on disk (R-2.12.1, R-2.12.3). A synced name
// that is gone from disk is the old name of a rename or a delete, not a
// rival, so no baseline lookup is needed.
//
// Each key holds the on-disk spellings under it.
type DirNames map[namekey.Key][]string

// dirNamesOf is the listing of entries.
func dirNamesOf(entries []os.DirEntry) DirNames {
	d := make(DirNames, len(entries))
	for _, e := range entries {
		d.Add(e.Name())
	}

	return d
}

// Add records disk, an entry's spelling on disk. Adding one spelling twice
// (a followed symlink visiting a folder again) records it once.
func (d DirNames) Add(disk string) {
	key := namekey.Of(disk)
	if slices.Contains(d[key], disk) {
		return
	}
	d[key] = append(d[key], disk)
}

// addUnlessStored records disk unless an entry with its stored form is
// already there, possibly in another spelling: a watch create reports the
// stored name of an entry the listing holds in its on-disk spelling.
func (d DirNames) addUnlessStored(disk string) {
	key := namekey.Of(disk)
	stored := namekey.Stored(disk)
	if slices.ContainsFunc(d[key], func(existing string) bool { return namekey.Stored(existing) == stored }) {
		return
	}
	d[key] = append(d[key], disk)
}

// remove drops the entry whose stored form is name's.
func (d DirNames) remove(name string) {
	key := namekey.Of(name)
	stored := namekey.Stored(name)
	names := d[key]
	for i, n := range names {
		if namekey.Stored(n) == stored {
			d[key] = append(names[:i], names[i+1:]...)
			if len(d[key]) == 0 {
				delete(d, key)
			}

			return
		}
	}
}

// spellings is every on-disk spelling OneDrive treats as name.
func (d DirNames) spellings(name string) []string {
	return d[namekey.Of(name)]
}

// rivals returns the other on-disk spellings that collide with the entry
// stored as name, and whether a second entry has name's stored form itself
// (an NFC and an NFD twin, one path here and one name on OneDrive, R-2.13.2).
func (d DirNames) rivals(name string) (others []string, sameForm bool) {
	stored := namekey.Stored(name)
	same := 0
	for _, existing := range d[namekey.Of(name)] {
		if namekey.Stored(existing) != stored {
			others = append(others, existing)
			continue
		}
		same++
	}

	return others, same > 1
}

// ScanNames is the on-disk names one full scan accepted, by the stored path
// of their directory.
type ScanNames map[string]DirNames

// Add records the entry stored at dbRelPath under its on-disk name.
func (s ScanNames) Add(dbRelPath, diskName string) {
	dir := filepath.Dir(dbRelPath)
	d := s[dir]
	if d == nil {
		d = make(DirNames)
		s[dir] = d
	}
	d.Add(diskName)
}

// removeSpelling drops the entry stored at dbRelPath whose on-disk name is
// diskName: an entry the walk accepted and then set aside (admitSyncedEntry).
func (s ScanNames) removeSpelling(dbRelPath, diskName string) {
	d := s[filepath.Dir(dbRelPath)]
	if d == nil {
		return
	}
	key := namekey.Of(diskName)
	if i := slices.Index(d[key], diskName); i >= 0 {
		d[key] = slices.Delete(d[key], i, i+1)
		if len(d[key]) == 0 {
			delete(d, key)
		}
	}
}
