// Package localtrash keeps what sync removes or overwrites on this computer
// for a while instead of destroying it.
//
// Every local delete the engine makes, and the old content of every file a
// download replaces, lands in one drive's trash under the data directory, not
// in the sync folder, so it is never observed or uploaded. Entries expire
// after Retention. The trash turns the data-loss outcomes that safety checks
// exist to prevent (a mass delete that slipped through, a file replaced
// between verification and removal, a remote deletion the user did not mean)
// into something a user can undo with `onedrive-go trash restore` (P-4).
//
// Layout, one directory per entry:
//
//	<trash dir>/<id>/item        the file itself
//	<trash dir>/<id>/entry.json  where it came from and why
//
// An entry directory without entry.json is an incomplete placement: List
// skips it and Purge removes it.
package localtrash

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

// Retention is how long an entry is kept before Purge removes it.
const Retention = 30 * 24 * time.Hour

const (
	itemName       = "item"
	entryFileName  = "entry.json"
	dirPerm        = 0o700
	filePerm       = 0o600
	idRandomBytes  = 4
	idTimeLayout   = "20060102T150405.000000000Z"
	stateDBSuffix  = ".db"
	trashDirName   = "trash"
	entryTempGlob  = ".entry-*"
	maxListEntries = 1 << 20
	parentDirName  = ".."

	// incompletePlacementGrace is how old an entry without entry.json must
	// be before Purge treats it as crash debris rather than a placement a
	// worker is still filling.
	incompletePlacementGrace = time.Hour
)

// ErrEntryNotFound reports a trash entry ID that does not exist.
var ErrEntryNotFound = errors.New("no such trash entry")

// ErrRestoreTargetExists reports that something already occupies the path a
// trash entry would be restored to; restoring never overwrites.
var ErrRestoreTargetExists = errors.New("a file already exists where the trash entry would be restored")

// Reason says why sync took a file out of the sync folder.
type Reason string

const (
	// ReasonDeleted: the item was deleted on OneDrive, so sync deleted it here.
	ReasonDeleted Reason = "deleted"
	// ReasonReplaced: a download replaced this version of the file.
	ReasonReplaced Reason = "replaced"
)

// Entry describes one trashed file.
type Entry struct {
	ID string `json:"id"`
	// Path is the file's path relative to SyncRoot, slash-separated.
	Path      string    `json:"path"`
	SyncRoot  string    `json:"sync_root"`
	Reason    Reason    `json:"reason"`
	Size      int64     `json:"size"`
	TrashedAt time.Time `json:"trashed_at"`
}

// Trash is one drive's trash directory.
type Trash struct {
	dir string
	now func() time.Time
}

// DirForStatePath returns the trash directory of the drive whose sync state
// lives at statePath: a sibling "trash" directory named after the state DB,
// so every mount (including shortcut children, which have their own state)
// has its own trash under the data directory.
func DirForStatePath(statePath string) string {
	if statePath == "" {
		return ""
	}
	base := strings.TrimSuffix(filepath.Base(statePath), stateDBSuffix)
	return filepath.Join(filepath.Dir(statePath), trashDirName, base)
}

// Open returns the trash at dir. Nothing is created until something is put
// in it.
func Open(dir string, now func() time.Time) *Trash {
	if now == nil {
		now = time.Now
	}
	return &Trash{dir: dir, now: now}
}

// Dir returns the trash directory.
func (t *Trash) Dir() string {
	return t.dir
}

// Placement is a reserved trash entry: the caller puts the file at ItemPath,
// then calls Commit, or Abandon if it could not.
type Placement struct {
	trash    *Trash
	entry    Entry
	entryDir string
	// ItemPath is where the caller must put the file.
	ItemPath string
}

// Reserve creates an empty entry for a file at rel under syncRoot.
func (t *Trash) Reserve(syncRoot string, rel string, reason Reason) (*Placement, error) {
	if t == nil || t.dir == "" {
		return nil, fmt.Errorf("trash directory is not configured")
	}
	now := t.now().UTC()
	var random [idRandomBytes]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, fmt.Errorf("naming trash entry: %w", err)
	}
	id := now.Format(idTimeLayout) + "-" + hex.EncodeToString(random[:])
	entryDir := filepath.Join(t.dir, id)
	if err := localpath.MkdirAll(entryDir, dirPerm); err != nil {
		return nil, fmt.Errorf("creating trash entry: %w", err)
	}

	return &Placement{
		trash:    t,
		entryDir: entryDir,
		ItemPath: filepath.Join(entryDir, itemName),
		entry: Entry{
			ID:        id,
			Path:      filepath.ToSlash(rel),
			SyncRoot:  syncRoot,
			Reason:    reason,
			TrashedAt: now,
		},
	}, nil
}

// Commit records the entry once its file is in place.
func (p *Placement) Commit() (Entry, error) {
	info, err := localpath.Lstat(p.ItemPath)
	if err != nil {
		return Entry{}, fmt.Errorf("checking trashed file: %w", err)
	}
	p.entry.Size = info.Size()
	data, err := json.MarshalIndent(p.entry, "", "  ")
	if err != nil {
		return Entry{}, fmt.Errorf("encoding trash entry: %w", err)
	}
	if err := localpath.AtomicWrite(filepath.Join(p.entryDir, entryFileName), data, filePerm, dirPerm, entryTempGlob); err != nil {
		return Entry{}, fmt.Errorf("recording trash entry: %w", err)
	}
	return p.entry, nil
}

// Abandon removes a reserved entry whose file never arrived.
func (p *Placement) Abandon() error {
	if err := localpath.RemoveAll(p.entryDir); err != nil {
		return fmt.Errorf("removing unused trash entry: %w", err)
	}
	return nil
}

// List returns the committed entries, newest first.
func (t *Trash) List() ([]Entry, error) {
	dirEntries, err := localpath.ReadDir(t.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading trash: %w", err)
	}

	entries := make([]Entry, 0, min(len(dirEntries), maxListEntries))
	for _, dirEntry := range dirEntries {
		if !dirEntry.IsDir() {
			continue
		}
		entry, ok, readErr := t.readEntry(dirEntry.Name())
		if readErr != nil {
			return nil, readErr
		}
		if ok {
			entries = append(entries, entry)
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		if !entries[i].TrashedAt.Equal(entries[j].TrashedAt) {
			return entries[i].TrashedAt.After(entries[j].TrashedAt)
		}
		return entries[i].ID > entries[j].ID
	})

	return entries, nil
}

func (t *Trash) readEntry(id string) (Entry, bool, error) {
	data, err := localpath.ReadFile(filepath.Join(t.dir, id, entryFileName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Entry{}, false, nil
		}
		return Entry{}, false, fmt.Errorf("reading trash entry %s: %w", id, err)
	}
	var entry Entry
	if err := json.Unmarshal(data, &entry); err != nil {
		return Entry{}, false, fmt.Errorf("decoding trash entry %s: %w", id, err)
	}
	entry.ID = id
	return entry, true, nil
}

// Restore puts entry id back at its original path and removes it from the
// trash. It never overwrites: an occupied path is ErrRestoreTargetExists.
func (t *Trash) Restore(id string) (string, error) {
	if id == "" || strings.ContainsAny(id, `/\`) || id == "." || id == parentDirName {
		return "", fmt.Errorf("%w: %q", ErrEntryNotFound, id)
	}
	entry, ok, err := t.readEntry(id)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrEntryNotFound, id)
	}

	target, err := restoreTarget(entry)
	if err != nil {
		return "", err
	}
	if _, statErr := localpath.Lstat(target); statErr == nil {
		return "", fmt.Errorf("%w: %s", ErrRestoreTargetExists, target)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return "", fmt.Errorf("checking %s: %w", target, statErr)
	}
	if err := localpath.MkdirAll(filepath.Dir(target), dirPerm); err != nil {
		return "", fmt.Errorf("creating %s: %w", filepath.Dir(target), err)
	}
	if err := localpath.Rename(filepath.Join(t.dir, id, itemName), target); err != nil {
		return "", fmt.Errorf("restoring %s: %w", target, err)
	}
	if err := localpath.RemoveAll(filepath.Join(t.dir, id)); err != nil {
		return target, fmt.Errorf("removing restored trash entry %s: %w", id, err)
	}

	return target, nil
}

// restoreTarget is the absolute original path of entry, refusing a recorded
// path that would leave its sync folder.
func restoreTarget(entry Entry) (string, error) {
	rel := filepath.FromSlash(entry.Path)
	if entry.SyncRoot == "" || rel == "" || filepath.IsAbs(rel) {
		return "", fmt.Errorf("trash entry %s has no usable original path", entry.ID)
	}
	clean := filepath.Clean(rel)
	if clean == "." || clean == parentDirName || strings.HasPrefix(clean, parentDirName+string(filepath.Separator)) {
		return "", fmt.Errorf("trash entry %s points outside its sync folder", entry.ID)
	}
	return filepath.Join(entry.SyncRoot, clean), nil
}

// Purge removes entries trashed longer than Retention ago, and incomplete
// placements a crash left behind. A placement younger than
// incompletePlacementGrace may still be in flight on a worker, so it is left
// alone. It returns how many committed entries it removed.
func (t *Trash) Purge() (int, error) {
	return t.removeWhere(func(entry Entry, complete bool, reservedAt time.Time) bool {
		if !complete {
			return t.now().Sub(reservedAt) > incompletePlacementGrace
		}
		return t.now().Sub(entry.TrashedAt) > Retention
	})
}

// Empty removes every committed entry. It returns how many it removed.
func (t *Trash) Empty() (int, error) {
	return t.removeWhere(func(_ Entry, complete bool, _ time.Time) bool { return complete })
}

// reservedAt is when an entry directory was reserved: the timestamp its ID
// starts with, on the trash's own clock, falling back to the directory's
// modification time for a name this package did not make.
func reservedAt(dirEntry os.DirEntry) time.Time {
	stamp, _, found := strings.Cut(dirEntry.Name(), "-")
	if found {
		if at, err := time.Parse(idTimeLayout, stamp); err == nil {
			return at
		}
	}
	if info, err := dirEntry.Info(); err == nil {
		return info.ModTime()
	}
	return time.Time{}
}

func (t *Trash) removeWhere(remove func(entry Entry, complete bool, reservedAt time.Time) bool) (int, error) {
	dirEntries, err := localpath.ReadDir(t.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("reading trash: %w", err)
	}

	removed := 0
	var errs []error
	for _, dirEntry := range dirEntries {
		if !dirEntry.IsDir() {
			continue
		}
		entry, complete, readErr := t.readEntry(dirEntry.Name())
		if readErr != nil {
			errs = append(errs, readErr)
			continue
		}
		if !remove(entry, complete, reservedAt(dirEntry)) {
			continue
		}
		if err := localpath.RemoveAll(filepath.Join(t.dir, dirEntry.Name())); err != nil {
			errs = append(errs, fmt.Errorf("removing trash entry %s: %w", dirEntry.Name(), err))
			continue
		}
		if complete {
			removed++
		}
	}

	return removed, errors.Join(errs...)
}
