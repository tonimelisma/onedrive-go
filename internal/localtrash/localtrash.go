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
// Reserve writes entry.json with state "pending" before anything is moved,
// and Commit rewrites it as "committed". A crash between the move and the
// commit therefore leaves a pending entry whose item is listed and can be
// restored; an entry directory with an item but no readable entry.json is an
// "orphan", listed with the location of its file. Purge expires only
// committed entries and never removes a file that is not committed: what it
// removes on its own is only reservations whose file never arrived.
package localtrash

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

// Retention is how long a committed entry is kept before Purge removes it.
const Retention = 30 * 24 * time.Hour

const (
	itemName      = "item"
	entryFileName = "entry.json"
	dirPerm       = 0o700
	filePerm      = 0o600
	idRandomBytes = 4
	idTimeLayout  = "20060102T150405.000000000Z"
	stateDBSuffix = ".db"
	trashDirName  = "trash"
	entryTempGlob = ".entry-*"
	// copyTempGlob names a cross-filesystem restore copy beside its target.
	// IsRestoreArtifactName recognizes it so sync never observes or uploads
	// one left behind by a crash.
	copyTempGlob          = restoreArtifactPrefix + "*"
	restoreArtifactPrefix = ".onedrive-go-restore-"
	maxListEntries        = 1 << 20
	parentDirName         = ".."

	// incompletePlacementGrace is how old a reservation without a file must
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

// State is where an entry is in its lifecycle.
type State string

const (
	// StatePending: reserved; the file may or may not have arrived. A
	// pending entry that holds a file is a crash between the move and the
	// commit, and is recoverable.
	StatePending State = "pending"
	// StateCommitted: the file is in place and recorded.
	StateCommitted State = "committed"
	// StateOrphan: a file with no readable record of where it came from.
	StateOrphan State = "orphan"
)

// Entry describes one trashed file.
type Entry struct {
	ID string `json:"id"`
	// Path is the file's path relative to SyncRoot, slash-separated. Empty
	// for an orphan.
	Path      string    `json:"path"`
	SyncRoot  string    `json:"sync_root"`
	Reason    Reason    `json:"reason"`
	Size      int64     `json:"size"`
	TrashedAt time.Time `json:"trashed_at"`
	State     State     `json:"state"`
	// ItemPath is where the file is inside the trash; not persisted.
	ItemPath string `json:"-"`
}

// Trash is one drive's trash directory.
type Trash struct {
	dir string
	now func() time.Time
	// renameNoReplace moves a file without replacing anything at the
	// destination; a seam so tests can model a cross-filesystem restore.
	renameNoReplace func(src, dst string) error
}

// IsRestoreArtifactName reports whether name is a temporary copy a restore
// writes beside its target.
func IsRestoreArtifactName(name string) bool {
	return strings.HasPrefix(name, restoreArtifactPrefix)
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
	return &Trash{dir: dir, now: now, renameNoReplace: localpath.RenameNoReplace}
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

// Reserve creates an entry for a file at rel under syncRoot and records it as
// pending before the caller moves anything, so a crash after the move still
// leaves a record of where the file came from.
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

	placement := &Placement{
		trash:    t,
		entryDir: entryDir,
		ItemPath: filepath.Join(entryDir, itemName),
		entry: Entry{
			ID:        id,
			Path:      filepath.ToSlash(rel),
			SyncRoot:  syncRoot,
			Reason:    reason,
			TrashedAt: now,
			State:     StatePending,
		},
	}
	if err := placement.writeEntry(); err != nil {
		if removeErr := localpath.RemoveAll(entryDir); removeErr != nil {
			return nil, errors.Join(err, removeErr)
		}
		return nil, err
	}

	return placement, nil
}

func (p *Placement) writeEntry() error {
	data, err := json.MarshalIndent(p.entry, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding trash entry: %w", err)
	}
	if err := localpath.AtomicWrite(filepath.Join(p.entryDir, entryFileName), data, filePerm, dirPerm, entryTempGlob); err != nil {
		return fmt.Errorf("recording trash entry: %w", err)
	}
	return nil
}

// Commit records the entry as complete once its file is in place. A failed
// commit leaves the pending record, which still lists and restores.
func (p *Placement) Commit() (Entry, error) {
	info, err := localpath.Lstat(p.ItemPath)
	if err != nil {
		return Entry{}, fmt.Errorf("checking trashed file: %w", err)
	}
	p.entry.Size = info.Size()
	p.entry.State = StateCommitted
	if err := p.writeEntry(); err != nil {
		return Entry{}, err
	}
	return p.entry, nil
}

// Abandon removes a reserved entry whose file never arrived. It refuses to
// remove an entry that holds a file.
func (p *Placement) Abandon() error {
	if _, err := localpath.Lstat(p.ItemPath); err == nil {
		return fmt.Errorf("not removing trash entry %s: it holds a file", p.entry.ID)
	}
	if err := localpath.RemoveAll(p.entryDir); err != nil {
		return fmt.Errorf("removing unused trash entry: %w", err)
	}
	return nil
}

// scannedEntry is one entry directory as found on disk.
type scannedEntry struct {
	entry      Entry
	hasRecord  bool
	hasItem    bool
	reservedAt time.Time
}

func (t *Trash) scan() ([]scannedEntry, error) {
	dirEntries, err := localpath.ReadDir(t.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading trash: %w", err)
	}

	scanned := make([]scannedEntry, 0, min(len(dirEntries), maxListEntries))
	for _, dirEntry := range dirEntries {
		if !dirEntry.IsDir() {
			continue
		}
		scanned = append(scanned, t.scanEntry(dirEntry))
	}
	return scanned, nil
}

func (t *Trash) scanEntry(dirEntry os.DirEntry) scannedEntry {
	id := dirEntry.Name()
	itemPath := filepath.Join(t.dir, id, itemName)
	found := scannedEntry{reservedAt: reservedAt(dirEntry)}
	// Only a definite "not there" is no item: an entry whose file cannot be
	// checked is never purged as empty.
	if _, err := localpath.Lstat(itemPath); err == nil || !errors.Is(err, os.ErrNotExist) {
		found.hasItem = true
	}
	entry, ok := t.readRecord(id)
	if ok {
		found.hasRecord = true
		found.entry = entry
	} else {
		found.entry = Entry{ID: id, State: StateOrphan, TrashedAt: found.reservedAt}
	}
	found.entry.ID = id
	found.entry.ItemPath = itemPath
	if found.entry.State == "" {
		found.entry.State = StateCommitted
	}
	return found
}

// readRecord reads an entry's entry.json; a missing or unreadable record is
// reported as absent, which makes a directory with a file an orphan.
func (t *Trash) readRecord(id string) (Entry, bool) {
	data, err := localpath.ReadFile(filepath.Join(t.dir, id, entryFileName))
	if err != nil {
		return Entry{}, false
	}
	var entry Entry
	if err := json.Unmarshal(data, &entry); err != nil {
		return Entry{}, false
	}
	return entry, true
}

// List returns every entry that holds a file, committed or not, newest
// first.
func (t *Trash) List() ([]Entry, error) {
	scanned, err := t.scan()
	if err != nil {
		return nil, err
	}

	entries := make([]Entry, 0, len(scanned))
	for i := range scanned {
		if scanned[i].hasItem {
			entries = append(entries, scanned[i].entry)
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

// Restore puts entry id back at its original path and removes it from the
// trash. It never overwrites (ErrRestoreTargetExists), never writes through
// a symbolic link inside the sync folder, and copies when the trash and the
// sync folder are on different filesystems.
func (t *Trash) Restore(id string) (string, error) {
	if id == "" || strings.ContainsAny(id, `/\`) || id == "." || id == parentDirName {
		return "", fmt.Errorf("%w: %q", ErrEntryNotFound, id)
	}
	found, err := t.findEntry(id)
	if err != nil {
		return "", err
	}
	if !found.hasRecord {
		return "", fmt.Errorf("trash entry %s has no record of where it came from; the file is at %s",
			id, found.entry.ItemPath)
	}

	target, err := restoreTarget(&found.entry)
	if err != nil {
		return "", err
	}
	if err := refuseSymlinkedAncestors(found.entry.SyncRoot, target); err != nil {
		return "", err
	}
	if err := localpath.MkdirAll(filepath.Dir(target), dirPerm); err != nil {
		return "", fmt.Errorf("creating %s: %w", filepath.Dir(target), err)
	}
	if err := t.moveNoReplace(found.entry.ItemPath, target); err != nil {
		return "", err
	}
	if err := localpath.RemoveAll(filepath.Join(t.dir, id)); err != nil {
		return target, fmt.Errorf("removing restored trash entry %s: %w", id, err)
	}

	return target, nil
}

// findEntry returns the scanned entry id if it holds a file.
func (t *Trash) findEntry(id string) (*scannedEntry, error) {
	dirEntries, err := localpath.ReadDir(t.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrEntryNotFound, id)
		}
		return nil, fmt.Errorf("reading trash: %w", err)
	}
	for _, dirEntry := range dirEntries {
		if dirEntry.Name() != id || !dirEntry.IsDir() {
			continue
		}
		scanned := t.scanEntry(dirEntry)
		if !scanned.hasItem {
			break
		}
		return &scanned, nil
	}
	return nil, fmt.Errorf("%w: %s", ErrEntryNotFound, id)
}

// moveNoReplace moves the trashed file to target without replacing anything
// there, copying through a temporary sibling of target when the move would
// cross filesystems.
func (t *Trash) moveNoReplace(itemPath, target string) error {
	err := t.renameNoReplace(itemPath, target)
	switch {
	case err == nil, errors.Is(err, localpath.ErrSourceNotRemoved):
		// The file is at target; a source left in the entry goes with the
		// entry directory.
		return nil
	case errors.Is(err, localpath.ErrTargetExists):
		return fmt.Errorf("%w: %s", ErrRestoreTargetExists, target)
	case !errors.Is(err, syscall.EXDEV):
		return fmt.Errorf("restoring %s: %w", target, err)
	}

	temp, err := copyToSibling(itemPath, target)
	if err != nil {
		return err
	}
	if err := localpath.RenameNoReplace(temp, target); err != nil {
		if errors.Is(err, localpath.ErrSourceNotRemoved) {
			// In place; the temporary copy is a leftover, removed best effort.
			_ = localpath.Remove(temp) //nolint:errcheck // the restore succeeded; a leftover copy is harmless
			if removeErr := localpath.Remove(itemPath); removeErr != nil {
				return fmt.Errorf("removing the trashed copy after restoring %s: %w", target, removeErr)
			}
			return nil
		}
		removeErr := localpath.Remove(temp)
		if errors.Is(err, localpath.ErrTargetExists) {
			return errors.Join(fmt.Errorf("%w: %s", ErrRestoreTargetExists, target), removeErr)
		}
		return errors.Join(fmt.Errorf("restoring %s: %w", target, err), removeErr)
	}
	if err := localpath.Remove(itemPath); err != nil {
		return fmt.Errorf("removing the trashed copy after restoring %s: %w", target, err)
	}
	return nil
}

// copyToSibling copies src into a new temporary file beside target, synced
// to disk, and returns its path.
func copyToSibling(src, target string) (tempPath string, err error) {
	in, err := localpath.OpenRegular(src)
	if err != nil {
		return "", fmt.Errorf("opening trashed file: %w", err)
	}
	defer in.Close() //nolint:errcheck // read-only source

	out, err := localpath.CreateTemp(filepath.Dir(target), copyTempGlob)
	if err != nil {
		return "", fmt.Errorf("creating restore copy: %w", err)
	}
	tempPath = out.Name()
	defer func() {
		if err != nil {
			_ = out.Close()                //nolint:errcheck // error path; the copy error is reported
			_ = localpath.Remove(tempPath) //nolint:errcheck // best-effort cleanup of a failed copy
		}
	}()
	if _, err = io.Copy(out, in); err != nil {
		return "", fmt.Errorf("copying trashed file: %w", err)
	}
	if err = out.Sync(); err != nil {
		return "", fmt.Errorf("syncing restore copy: %w", err)
	}
	info, err := in.Stat()
	if err != nil {
		return "", fmt.Errorf("stating trashed file: %w", err)
	}
	if err = out.Chmod(info.Mode().Perm()); err != nil {
		return "", fmt.Errorf("setting restore copy permissions: %w", err)
	}
	if err = out.Close(); err != nil {
		return "", fmt.Errorf("closing restore copy: %w", err)
	}
	if err = localpath.Chtimes(tempPath, info.ModTime(), info.ModTime()); err != nil {
		return "", fmt.Errorf("setting restore copy times: %w", err)
	}
	return tempPath, nil
}

// refuseSymlinkedAncestors refuses a restore whose target folder, below the
// sync folder, passes through a symbolic link: following one would put the
// file outside the sync folder.
func refuseSymlinkedAncestors(syncRoot, target string) error {
	rel, err := filepath.Rel(syncRoot, filepath.Dir(target))
	if err != nil || rel == "." {
		return nil //nolint:nilerr // the target folder is the sync folder itself
	}
	current := syncRoot
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, statErr := localpath.Lstat(current)
		if statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				return nil
			}
			return fmt.Errorf("checking %s: %w", current, statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("not restoring through %s: it is a symbolic link", current)
		}
	}
	return nil
}

// restoreTarget is the absolute original path of entry, refusing a recorded
// path that would leave its sync folder.
func restoreTarget(entry *Entry) (string, error) {
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

// Purge removes committed entries older than Retention and reservations whose
// file never arrived. It never removes a file that was not committed: a
// pending or orphan entry holding one is kept for the user. It returns how
// many committed entries it removed.
func (t *Trash) Purge() (int, error) {
	return t.removeWhere(func(found scannedEntry) bool {
		if found.hasItem {
			return found.entry.State == StateCommitted && t.now().Sub(found.entry.TrashedAt) > Retention
		}
		if found.entry.State == StateCommitted {
			return t.now().Sub(found.entry.TrashedAt) > Retention
		}
		return t.now().Sub(found.reservedAt) > incompletePlacementGrace
	})
}

// Empty removes every entry that holds a file, committed or not. It returns
// how many it removed.
func (t *Trash) Empty() (int, error) {
	return t.removeWhere(func(found scannedEntry) bool { return found.hasItem })
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

func (t *Trash) removeWhere(remove func(found scannedEntry) bool) (int, error) {
	scanned, err := t.scan()
	if err != nil {
		return 0, err
	}

	removed := 0
	var errs []error
	for i := range scanned {
		if !remove(scanned[i]) {
			continue
		}
		if err := localpath.RemoveAll(filepath.Join(t.dir, scanned[i].entry.ID)); err != nil {
			errs = append(errs, fmt.Errorf("removing trash entry %s: %w", scanned[i].entry.ID, err))
			continue
		}
		if scanned[i].hasItem {
			removed++
		}
	}

	return removed, errors.Join(errs...)
}
