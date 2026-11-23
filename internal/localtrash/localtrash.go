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
//
// A shortcut child mount's trash belongs to its parent drive. It lives beside
// the parent's own trash, under the parent's name (ChildDir), so the parent's
// trash commands and housekeeping reach it (Children) whether or not the
// child mount still runs, and when the child's state is removed its entries
// move into the parent's trash as they are (HandOverTo).
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
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// Retention is how long a committed entry is kept before Purge removes it.
const Retention = 30 * 24 * time.Hour

const (
	itemName      = "item"
	entryFileName = "entry.json"
	entryDirPerm  = 0o700
	entryFilePerm = 0o600
	idRandomBytes = 4
	idTimeLayout  = "20060102T150405.000000000Z"
	stateDBSuffix = ".db"
	trashDirName  = "trash"
	// childrenDirName is the folder, beside the drives' trash folders, that
	// holds the trash folders of shortcut child mounts, one folder per parent
	// trash (ChildDir). A drive's trash folder is named after its state DB,
	// whose name starts with "state_", so this name is never a drive's.
	childrenDirName = "children"
	entryTempGlob   = ".entry-*"
	// restoreArtifactPrefix starts the name of a cross-filesystem restore
	// copy beside its target. IsRestoreArtifactName recognizes it so sync
	// never observes or uploads one left behind by a crash.
	restoreArtifactPrefix = ".onedrive-go-restore-"
	// restoreCopyAttempts bounds the random names a restore copy tries; a
	// taken name means a copy a crash left behind, or another writer, holds
	// exactly that name.
	restoreCopyAttempts    = 100
	restoreCopyRandomBytes = 8
	// restoreCopyPerm keeps a restore copy private until it takes the
	// trashed file's mode.
	restoreCopyPerm = 0o600
	parentDirName   = ".."

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

// ErrRestoreLeftover reports a restore that finished but left its temporary
// copy in the sync folder under a name sync never looks at: the file is
// restored, the entry leaves the trash as for a clean restore, and the error
// names the copy, which the user can delete.
var ErrRestoreLeftover = errors.New("restored, but its temporary copy could not be removed; you can delete it")

// ErrRestoreNotDurable reports a restore that placed the file but could not
// make it durable: a power cut could still lose the restored name, so the
// trash keeps its copy and the entry stays whole, and the error says where
// the file is and which entry holds the copy.
var ErrRestoreNotDurable = errors.New("restored, but the file could not be made durable")

// Reason says why sync took a file out of the sync folder.
type Reason string

const (
	// ReasonDeleted: the item was deleted on OneDrive, so sync deleted it here.
	ReasonDeleted Reason = "deleted"
	// ReasonReplaced: a download replaced this version of the file.
	ReasonReplaced Reason = "replaced"
	// ReasonReleased: the shared folder the file was in was removed from
	// this account, so sync released its synced copy here. The file was not
	// deleted on OneDrive: the sharer's copy still exists.
	ReasonReleased Reason = "released"
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
//
// The trash is a folder the client manages under its data directory, outside
// every sync folder, so its own side is reached through localpath by path:
// sync never observes it, no name in it comes from OneDrive or from the sync
// folder (entry names are ones this package generates), and its folders are
// created private to the user (entryDirPerm), so a symlink in it would have to
// be planted by the user's own account. Everything a restore changes inside a
// sync folder goes through that folder's rooted capability (synctree) instead.
type Trash struct {
	dir string
	now func() time.Time
	// moveIn moves a trashed file into a sync folder without replacing
	// anything at its destination; a seam so tests can model a
	// cross-filesystem restore.
	moveIn func(tree *synctree.Root, itemPath, rel string) error
	// renameIn gives a cross-filesystem restore copy its name inside the
	// sync folder without replacing anything or copying it a second time
	// (synctree.Root.RenameNoReplace); a seam so tests can model a
	// rename that put the copy in place but could not remove its temporary
	// name.
	renameIn func(tree *synctree.Root, srcRel, dstRel string) error
	// syncDir flushes the folder of the sync tree a cross-filesystem
	// restore renamed its copy into; a seam because an fsync has no
	// observable effect to test.
	syncDir func(tree *synctree.Root, rel string) error
	// openTree opens the sync folder a restore puts an entry back into; a
	// seam so tests can put an emulated filesystem under it
	// (synctree.Root.Interpose).
	openTree func(dir string) (*synctree.Root, error)
	// writeRecord writes an entry's record atomically; a seam so tests in
	// other packages can make a reservation fail (WithRecordWriteFailing).
	writeRecord func(path string, data []byte) error
	// renameEntry moves one entry directory to another trash (HandOverTo);
	// a seam so tests can stop a hand-over part way.
	renameEntry func(src, dst string) error
}

// writeRecordAtomically writes the record at path through a synced temporary
// sibling, creating the entry's folder if it is missing.
func writeRecordAtomically(path string, data []byte) error {
	return localpath.AtomicWrite(path, data, entryFilePerm, entryDirPerm, entryTempGlob) //nolint:wrapcheck // writeEntry names the record
}

// WithRecordWriteFailing returns a Trash over the same directory whose entry
// records cannot be written: each write fails with err. A reservation then
// fails as one does whose entry folder is removed while its record is being
// written, when err matches os.ErrNotExist. It is exported for tests in other
// packages, as synctree.Root.Interpose is.
func (t *Trash) WithRecordWriteFailing(err error) *Trash {
	failing := *t
	failing.writeRecord = func(string, []byte) error { return err }

	return &failing
}

// IsRestoreArtifactName reports whether name is a temporary copy a restore
// writes beside its target.
func IsRestoreArtifactName(name string) bool {
	return strings.HasPrefix(name, restoreArtifactPrefix)
}

// DirForStatePath returns the trash directory of the drive whose sync state
// lives at statePath: a sibling "trash" directory named after the state DB,
// so every configured drive has its own trash under the data directory. A
// shortcut child mount's trash is its parent's ChildDir instead.
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
	return &Trash{
		dir:         dir,
		now:         now,
		moveIn:      (*synctree.Root).MoveInNoReplace,
		renameIn:    (*synctree.Root).RenameNoReplace,
		syncDir:     (*synctree.Root).SyncDir,
		openTree:    synctree.Open,
		writeRecord: writeRecordAtomically,
		renameEntry: localpath.Rename,
	}
}

// ChildDir returns the trash directory of the shortcut child mount whose sync
// state lives at childStatePath, a child of the drive whose trash directory is
// parentDir. It is outside the parent's own entries, under the parent's name,
// so the parent's Children finds it.
func ChildDir(parentDir, childStatePath string) string {
	if parentDir == "" || childStatePath == "" {
		return ""
	}
	return filepath.Join(childrenContainer(parentDir), strings.TrimSuffix(filepath.Base(childStatePath), stateDBSuffix))
}

// childrenContainer is the folder that holds the trash directories of the
// child mounts of the drive whose trash directory is parentDir.
func childrenContainer(parentDir string) string {
	return filepath.Join(filepath.Dir(parentDir), childrenDirName, filepath.Base(parentDir))
}

// Children opens the trash of every shortcut child mount of this drive that
// has a trash directory (ChildDir), in the order of their directory names,
// on this trash's clock. A drive with no child trash has none.
func (t *Trash) Children() ([]*Trash, error) {
	if t == nil || t.dir == "" {
		return nil, nil
	}
	container := childrenContainer(t.dir)
	dirEntries, err := localpath.ReadDir(container)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading the trash of shortcut children: %w", err)
	}
	children := make([]*Trash, 0, len(dirEntries))
	for _, dirEntry := range dirEntries {
		if dirEntry.IsDir() {
			children = append(children, Open(filepath.Join(container, dirEntry.Name()), t.now))
		}
	}
	return children, nil
}

// HandOverTo moves every entry of this trash into dst, then removes this
// trash's directory. It is for a shortcut child mount whose state is being
// removed: its entries become its parent drive's, which the parent's
// housekeeping expires after Retention like its own. It returns how many
// entries it moved.
//
// Each entry moves whole, as one rename of its directory inside the data
// directory, so it keeps its ID, record, state and file, and at every moment
// it is in exactly one of the two trashes, whatever it holds: a committed
// entry, a pending one, an orphan, a reservation whose file never arrived. A
// crash leaves the entries not yet moved where they were, and HandOverTo run
// again moves them. The renames are flushed in both directories before this
// trash's directory goes, so a power cut cannot lose a moved entry's new name
// after the caller has removed what pointed at the old one.
//
// It never removes an entry. One it cannot move, or whose ID dst already
// holds, stays here and is reported, and so does this trash's directory while
// anything is left in it: the caller keeps what points at this trash and
// tries again. Entry IDs are a nanosecond time and random bytes, so a taken
// ID is not another entry being reserved at that moment.
func (t *Trash) HandOverTo(dst *Trash) (int, error) {
	if t == nil || t.dir == "" {
		return 0, nil
	}
	if dst == nil || dst.dir == "" {
		return 0, fmt.Errorf("handing over trash %s: the receiving trash directory is not configured", t.dir)
	}
	dirEntries, err := localpath.ReadDir(t.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("reading trash: %w", err)
	}
	if len(dirEntries) > 0 {
		if err := localpath.MkdirAll(dst.dir, entryDirPerm); err != nil {
			return 0, fmt.Errorf("creating the receiving trash: %w", err)
		}
	}

	moved := 0
	var errs error
	for _, dirEntry := range dirEntries {
		if err := t.handOverEntry(dst, dirEntry); err != nil {
			errs = localpath.JoinOnOneLine(errs, err)
			continue
		}
		moved++
	}
	if moved > 0 {
		errs = localpath.JoinOnOneLine(errs, syncHandOverDir(dst.dir))
		errs = localpath.JoinOnOneLine(errs, syncHandOverDir(t.dir))
	}
	if errs != nil {
		return moved, errs
	}
	if err := localpath.Remove(t.dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return moved, fmt.Errorf("removing the handed-over trash folder: %w", err)
	}

	return moved, nil
}

// handOverEntry moves the entry directory dirEntry of t into dst under the
// same name, refusing a name dst already holds and anything that is not an
// entry directory.
func (t *Trash) handOverEntry(dst *Trash, dirEntry os.DirEntry) error {
	src := filepath.Join(t.dir, dirEntry.Name())
	if !dirEntry.IsDir() {
		return fmt.Errorf("not handing over %s: it is not a trash entry", src)
	}
	target := filepath.Join(dst.dir, dirEntry.Name())
	_, err := localpath.Lstat(target)
	switch {
	case err == nil:
		return fmt.Errorf("not handing over trash entry %s: %s already holds an entry with that ID", dirEntry.Name(), dst.dir)
	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("checking %s: %w", target, err)
	}
	if err := t.renameEntry(src, target); err != nil {
		return fmt.Errorf("handing over trash entry %s: %w", dirEntry.Name(), err)
	}
	return nil
}

// syncHandOverDir flushes a trash directory a hand-over renamed an entry into
// or out of; a filesystem that cannot sync a directory is tolerated.
func syncHandOverDir(dir string) error {
	if err := localpath.SyncDir(dir); err != nil && !localpath.IsDirSyncUnsupported(err) {
		return fmt.Errorf("flushing trash folder %s: %w", dir, err)
	}
	return nil
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
	if err := localpath.MkdirAll(entryDir, entryDirPerm); err != nil {
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
			return nil, localpath.JoinOnOneLine(err, removeErr)
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
	if err := p.trash.writeRecord(filepath.Join(p.entryDir, entryFileName), data); err != nil {
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
	if held := heldFileIn(p.entryDir); held != nil {
		return fmt.Errorf("not removing trash entry %s: it holds a file at %s", p.entry.ID, held.path)
	}
	if err := localpath.RemoveAll(p.entryDir); err != nil {
		return fmt.Errorf("removing unused trash entry: %w", err)
	}
	return nil
}

// heldFile is the file an entry directory holds.
type heldFile struct {
	path string
	// incomplete marks a file that is not the item: a `.copy-*` a
	// cross-filesystem placement was writing when the run died. It may be
	// torn, so it is kept and listed but never restored as the file.
	incomplete bool
}

// heldFileIn returns the file an entry directory holds: the item, or, when
// there is none, any other regular file but the record. nil means definitely
// none. Only a definite "nothing there" is nothing, an entry directory that
// is gone included: an entry whose contents cannot be checked is reported as
// holding its item, so it is never purged as empty.
func heldFileIn(entryDir string) *heldFile {
	itemPath := filepath.Join(entryDir, itemName)
	if _, err := localpath.Lstat(itemPath); err == nil || !errors.Is(err, os.ErrNotExist) {
		return &heldFile{path: itemPath}
	}
	dirEntries, err := localpath.ReadDir(entryDir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return &heldFile{path: itemPath}
	}
	for _, dirEntry := range dirEntries {
		if dirEntry.Name() != entryFileName && dirEntry.Type().IsRegular() {
			return &heldFile{path: filepath.Join(entryDir, dirEntry.Name()), incomplete: true}
		}
	}
	return nil
}

// scannedEntry is one entry directory as found on disk.
type scannedEntry struct {
	entry     Entry
	hasRecord bool
	hasItem   bool
	// itemIncomplete: the file held is an interrupted copy, not the item.
	itemIncomplete bool
	reservedAt     time.Time
}

func (t *Trash) scan() ([]scannedEntry, error) {
	dirEntries, err := localpath.ReadDir(t.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading trash: %w", err)
	}

	scanned := make([]scannedEntry, 0, len(dirEntries))
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
	if held := heldFileIn(filepath.Join(t.dir, id)); held != nil {
		found.hasItem = true
		found.itemIncomplete = held.incomplete
		itemPath = held.path
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
// a symbolic link inside the sync folder, never re-creates a sync folder that
// is gone, and copies when the trash and the sync folder are on different
// filesystems. Everything it changes in the sync folder goes through the
// folder's rooted capability (putBack).
//
// A non-empty destination means the file is at that path, whatever the error
// says, and the error says what else went wrong. Once the file is placed,
// Restore ends the entry in this order, and each failure stops it there: the
// restored file is made durable, then the trashed file is removed, then the
// record and the entry's directory. A file that could not be made durable
// has an error matching ErrRestoreNotDurable, and the trash keeps the entry
// whole; a trashed file that could not be removed keeps its record too, so
// the entry stays listed. Restoring such an entry again finds the file in
// the way (ErrRestoreTargetExists). A restore that finished but could not
// remove its temporary copy has an error matching ErrRestoreLeftover that
// names the copy.
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
	if found.itemIncomplete {
		return "", fmt.Errorf("trash entry %s holds only a copy an interrupted run was still writing, at %s; "+
			"the original was left set aside in the sync folder", id, found.entry.ItemPath)
	}

	dest, err := restoreTarget(&found.entry)
	if err != nil {
		return "", err
	}
	if err := requireSyncFolder(&found.entry); err != nil {
		return "", err
	}
	if err := refuseSymlinkedAncestors(found.entry.SyncRoot, dest.abs); err != nil {
		return "", err
	}
	tree, openErr := t.openTree(found.entry.SyncRoot)
	if openErr != nil {
		return "", fmt.Errorf("opening the sync folder of trash entry %s: %w", id, openErr)
	}
	// A put-back that placed the file returns its path with any error that
	// came after; only a durable file lets the entry go.
	restored, putBackErr := t.putBack(tree, found.entry.ItemPath, dest)
	switch {
	case restored == "":
		return "", putBackErr
	case errors.Is(putBackErr, ErrRestoreNotDurable):
		return restored, fmt.Errorf("%w; the trash keeps its copy as entry %s", putBackErr, id)
	}
	if err := t.removeRestoredEntry(id, found.entry.ItemPath, restored); err != nil {
		return restored, localpath.JoinOnOneLine(putBackErr, err)
	}

	return restored, putBackErr
}

// removeRestoredEntry takes entry id out of the trash once its file is
// durable at restored: the trashed file at itemPath first, then the record
// and the entry's directory, so the record never goes while the trashed file
// is there. A trashed file already gone, which the move took, is no failure;
// one that cannot be removed stops the removal with the record kept, so the
// entry stays listed and whole.
func (t *Trash) removeRestoredEntry(id, itemPath, restored string) error {
	if err := localpath.Remove(itemPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("restored %s, but its trashed copy could not be removed and stays in the trash as entry %s: %w",
			restored, id, err)
	}
	if err := localpath.RemoveAll(filepath.Join(t.dir, id)); err != nil {
		return fmt.Errorf("removing restored trash entry %s: %w", id, err)
	}

	return nil
}

// restoreDest is where a trash entry goes back to: rel inside its sync
// folder, which every change is made through, and abs, the same place as the
// user is told it.
type restoreDest struct {
	rel string
	abs string
}

// putBack moves the trashed file at itemPath to dest inside tree. Every change
// in the sync folder goes through tree, the folder's rooted capability: the
// missing parents are made through it without following a symbolic link,
// even one that stays inside the sync folder, the move is its no-replace
// rename, and across filesystems the copy is created, timed and named through
// it. A folder on the way swapped after Restore's own check for a symbolic
// link out of the root, or for anything that is not a folder, therefore fails
// the restore instead of placing the file where the link leads. It returns
// dest's path once the file is there, with any error that came after, and ""
// while it is not.
func (t *Trash) putBack(tree *synctree.Root, itemPath string, dest restoreDest) (string, error) {
	// The missing parents are in the user's sync folder, not in the trash:
	// the sync tree gives them the user-tree folder mode, not the trash's own.
	if err := tree.MkdirAllNoFollow(filepath.Dir(dest.rel)); err != nil {
		return "", fmt.Errorf("creating %s: %w", filepath.Dir(dest.abs), err)
	}

	return t.moveNoReplace(tree, itemPath, dest)
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

// moveNoReplace moves the trashed file to dest without replacing anything
// there, copying through a temporary sibling of dest when the move would
// cross filesystems. It returns dest's path once the file is there, as
// putBack does.
func (t *Trash) moveNoReplace(tree *synctree.Root, itemPath string, dest restoreDest) (string, error) {
	err := t.moveIn(tree, itemPath, dest.rel)
	switch {
	case err == nil, errors.Is(err, synctree.ErrSourceNotRemoved):
		// The file is at dest; Restore removes a source left in the entry
		// before the entry's record.
		return dest.abs, nil
	case errors.Is(err, synctree.ErrTargetExists):
		return "", fmt.Errorf("%w: %s", ErrRestoreTargetExists, dest.abs)
	case !errors.Is(err, syscall.EXDEV):
		return "", fmt.Errorf("restoring %s: %w", dest.abs, err)
	}

	tempRel, err := copyIntoTree(tree, itemPath, filepath.Dir(dest.rel))
	if err != nil {
		return "", err
	}
	if err := t.renameIn(tree, tempRel, dest.rel); err != nil {
		if errors.Is(err, synctree.ErrSourceNotRemoved) {
			return t.finishLeftoverRestore(tree, dest, tempRel)
		}
		removeErr := tree.Remove(tempRel)
		if errors.Is(err, synctree.ErrTargetExists) {
			return "", localpath.JoinOnOneLine(fmt.Errorf("%w: %s", ErrRestoreTargetExists, dest.abs), removeErr)
		}
		return "", localpath.JoinOnOneLine(fmt.Errorf("restoring %s: %w", dest.abs, err), removeErr)
	}
	return t.finishCopiedRestore(tree, dest)
}

// finishCopiedRestore makes a copied restore durable before Restore removes
// the trashed file: the rename that named the copy is a directory update,
// not durable until the folder is synced, and removing the trashed file
// first would leave a power cut with neither. A folder the filesystem cannot
// sync is tolerated. The file is at dest whatever happens here, so dest's
// path is returned, with an error matching ErrRestoreNotDurable when the
// folder could not be synced.
func (t *Trash) finishCopiedRestore(tree *synctree.Root, dest restoreDest) (string, error) {
	if err := t.syncDir(tree, filepath.Dir(dest.rel)); err != nil && !localpath.IsDirSyncUnsupported(err) {
		return dest.abs, fmt.Errorf("%w: it is at %s: %w", ErrRestoreNotDurable, dest.abs, err)
	}
	return dest.abs, nil
}

// finishLeftoverRestore is finishCopiedRestore for a copy that got its name
// at dest while its temporary name tempRel could not be removed: the two
// names are one file. tempRel is removed again, and one that still cannot be
// is reported, since sync never sees a restore artifact. Once the restore is
// otherwise complete the report matches ErrRestoreLeftover; when
// finishCopiedRestore fails, its failure, still matching
// ErrRestoreNotDurable, names the leftover beside its reason as text. Either
// way the file is at dest, and dest's path is returned.
func (t *Trash) finishLeftoverRestore(tree *synctree.Root, dest restoreDest, tempRel string) (string, error) {
	var leftoverErr error
	if err := tree.Remove(tempRel); err != nil && !errors.Is(err, os.ErrNotExist) {
		leftoverErr = fmt.Errorf("%s: %w", filepath.Join(filepath.Dir(dest.abs), filepath.Base(tempRel)), err)
	}
	restored, err := t.finishCopiedRestore(tree, dest)
	switch {
	case err != nil && leftoverErr != nil:
		return restored, fmt.Errorf("%w; its temporary copy was left too: %s", err, leftoverErr.Error())
	case err != nil:
		return restored, err
	case leftoverErr != nil:
		return restored, fmt.Errorf("%w: %w", ErrRestoreLeftover, leftoverErr)
	default:
		return restored, nil
	}
}

// copyIntoTree copies the trashed file src into a new temporary file in the
// folder dirRel of tree, with the file's mode and modification time, synced
// to disk, and returns the copy's name relative to tree. The copy is created
// exclusively through the sync folder's rooted capability, and its times are
// set through it too, on the copy's name (Chtimes resolves that name inside
// the sync folder, to the nanosecond), so they go to whatever entry inside
// the sync folder holds that name at that moment. A copy that fails is
// removed.
func copyIntoTree(tree *synctree.Root, src, dirRel string) (string, error) {
	in, err := localpath.OpenRegular(src)
	if err != nil {
		return "", fmt.Errorf("opening trashed file: %w", err)
	}
	defer in.Close() //nolint:errcheck // read-only source
	info, err := in.Stat()
	if err != nil {
		return "", fmt.Errorf("stating trashed file: %w", err)
	}

	out, copyRel, err := createRestoreCopy(tree, dirRel)
	if err != nil {
		return "", err
	}
	if err := writeRestoreCopy(tree, out, copyRel, in, info); err != nil {
		return "", discardRestoreCopy(tree, out, copyRel, err)
	}

	return copyRel, nil
}

// writeRestoreCopy fills out, the new restore copy copyRel, from in, gives it
// the mode and modification time of info, syncs it and closes it.
func writeRestoreCopy(tree *synctree.Root, out *os.File, copyRel string, in io.Reader, info os.FileInfo) error {
	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("copying trashed file: %w", err)
	}
	if err := out.Chmod(info.Mode().Perm()); err != nil {
		return fmt.Errorf("setting restore copy permissions: %w", err)
	}
	if err := tree.Chtimes(copyRel, info.ModTime(), info.ModTime()); err != nil {
		return fmt.Errorf("setting restore copy times: %w", err)
	}
	if err := out.Sync(); err != nil {
		return fmt.Errorf("syncing restore copy: %w", err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("closing restore copy: %w", err)
	}

	return nil
}

// discardRestoreCopy closes out, the partial restore copy copyRel that cause
// ended, and removes it through tree. A copy left behind is a restore
// artifact that sync never sees, so a close or removal that fails is joined
// to cause for the user; a copy already closed, or already gone, is no
// further failure.
func discardRestoreCopy(tree *synctree.Root, out *os.File, copyRel string, cause error) error {
	result := cause
	if err := out.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		result = localpath.JoinOnOneLine(result, fmt.Errorf("closing the partial restore copy %s: %w", copyRel, err))
	}
	if err := tree.Remove(copyRel); err != nil && !errors.Is(err, os.ErrNotExist) {
		result = localpath.JoinOnOneLine(result, fmt.Errorf("removing the partial restore copy %s: %w", copyRel, err))
	}

	return result
}

// createRestoreCopy creates an empty restore copy in dirRel of tree under a
// fresh random name. The create is exclusive, so a name another writer holds,
// or a symlink planted at it, is left alone and another name tried.
func createRestoreCopy(tree *synctree.Root, dirRel string) (*os.File, string, error) {
	for range restoreCopyAttempts {
		var random [restoreCopyRandomBytes]byte
		if _, err := rand.Read(random[:]); err != nil {
			return nil, "", fmt.Errorf("naming restore copy: %w", err)
		}
		rel := filepath.Join(dirRel, restoreArtifactPrefix+hex.EncodeToString(random[:]))
		out, err := tree.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, restoreCopyPerm)
		if err == nil {
			return out, rel, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, "", fmt.Errorf("creating restore copy: %w", err)
		}
	}

	return nil, "", fmt.Errorf("creating restore copy: no free name in %s after %d attempts", dirRel, restoreCopyAttempts)
}

// refuseSymlinkedAncestors refuses a restore whose target folder, below the
// sync folder, passes through a symbolic link: following one would put the
// file outside the sync folder. It is an early refusal that names the link,
// not the protection: a link swapped in after it is refused by the sync
// folder's rooted capability that every change of the restore goes through,
// with a less direct message.
func refuseSymlinkedAncestors(syncRoot, target string) error {
	rel, err := filepath.Rel(syncRoot, filepath.Dir(target))
	if err != nil {
		return fmt.Errorf("not restoring %s: it is not inside its sync folder %s: %w", target, syncRoot, err)
	}
	if rel == "." {
		return nil
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

// requireSyncFolder refuses a restore into a sync folder that is gone.
// Creating it again would make a new folder the drive's sync then refuses as
// not the one its state belongs to, and the client creates a sync folder only
// when a drive is set up, with the sync folder's own mode.
func requireSyncFolder(entry *Entry) error {
	info, err := localpath.Stat(entry.SyncRoot)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("not restoring trash entry %s: its sync folder %s does not exist; the file is at %s",
			entry.ID, entry.SyncRoot, entry.ItemPath)
	case err != nil:
		return fmt.Errorf("checking the sync folder of trash entry %s: %w", entry.ID, err)
	case !info.IsDir():
		return fmt.Errorf("not restoring trash entry %s: its sync folder %s is not a folder; the file is at %s",
			entry.ID, entry.SyncRoot, entry.ItemPath)
	default:
		return nil
	}
}

// restoreTarget is the original place of entry, refusing a recorded path that
// would leave its sync folder.
func restoreTarget(entry *Entry) (restoreDest, error) {
	rel := filepath.FromSlash(entry.Path)
	if entry.SyncRoot == "" || rel == "" || filepath.IsAbs(rel) {
		return restoreDest{}, fmt.Errorf("trash entry %s has no usable original path", entry.ID)
	}
	clean := filepath.Clean(rel)
	if clean == "." || clean == parentDirName || strings.HasPrefix(clean, parentDirName+string(filepath.Separator)) {
		return restoreDest{}, fmt.Errorf("trash entry %s points outside its sync folder", entry.ID)
	}
	return restoreDest{rel: clean, abs: filepath.Join(entry.SyncRoot, clean)}, nil
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
	var errs error
	for i := range scanned {
		if !remove(scanned[i]) {
			continue
		}
		if err := localpath.RemoveAll(filepath.Join(t.dir, scanned[i].entry.ID)); err != nil {
			errs = localpath.JoinOnOneLine(errs, fmt.Errorf("removing trash entry %s: %w", scanned[i].entry.ID, err))
			continue
		}
		if scanned[i].hasItem {
			removed++
		}
	}

	return removed, errs
}
