package sync

import (
	"path/filepath"
	"strings"
	"sync"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/namekey"
)

// skippedItem records a local filesystem entry that was rejected at
// observation time. The scanner collects these alongside events so the
// engine can persist them as durable observation issues.
type skippedItem struct {
	Path               string // NFC-normalized, relative to sync root
	Reason             string // issue type constant (IssueInvalidFilename, etc.)
	Detail             string // human-readable explanation
	FileSize           int64  // populated for IssueFileTooLarge (after stat)
	BlocksReadBoundary bool   // true when observation proved an unreadable subtree boundary
}

// changeEvent is an immutable observation fact produced by observers. The
// runtime uses these facts to mark coarse dirty/full-refresh signals and to describe local or
// remote observation results, but they are not the durable sync truth and are
// never stored in the database.
//
// Per-observer field contract:
//
//	RemoteObserver populates: all fields (ItemID, ParentID, DriveID, ETag,
//	  CTag, Hash, Size, Mtime). For ChangeDelete: Hash is empty; Size/Mtime
//	  from API response.
//	LocalObserver populates: Path, Name, ItemType, Size, Hash, Mtime,
//	  filesystem identity when available, and IsDeleted. Never sets: ItemID,
//	  ParentID, DriveID, ETag, CTag.
//	  For ChangeDelete: Hash is empty; Size/Mtime from baseline entry.
type changeEvent struct {
	Source   changeSource
	Type     changeType
	Path     string     // NFC-normalized, relative to sync root
	OldPath  string     // for moves only
	ItemID   string     // server-assigned (remote only; empty for local)
	ParentID string     // server parent ID (remote only)
	DriveID  driveid.ID // normalized (lowercase, zero-padded to 16 chars)
	ItemType ItemType
	Name     string // verbatim from Graph, NFC-normalized
	Size     int64
	// SizeUnknown marks a remote size Graph reported as negative: it is
	// stored as unknown (NULL), never as a size (R-2.4.16).
	SizeUnknown bool
	// Hash is a QuickXorHash (base64) on both sides: driveops.SelectHash
	// returns only QuickXor, so a remote item with SHA digests alone is
	// hashless here, never compared against a local QuickXor. Empty for
	// folders.
	Hash             string
	Mtime            int64 // Unix nanoseconds
	LocalCtime       int64 // local inode change time, files only; zero means unknown
	LocalDevice      uint64
	LocalInode       uint64
	LocalHasIdentity bool
	ETag             string // remote only
	CTag             string // remote only
	IsDeleted        bool
	// ReuploadLocal marks a synthesized remote delete for an item a resync
	// asked to be uploaded again (R-2.15.3): its synced agreement is
	// forgotten, so a surviving local copy is a new upload, never a delete.
	ReuploadLocal bool
}

// BaselineEntry represents the confirmed synced state of a single path.
// This is the ONLY durable per-item state in the system.
type BaselineEntry struct {
	Path            string
	DriveID         driveid.ID
	ItemID          string
	ParentID        string
	ItemType        ItemType
	LocalHash       string // per-side: handles SharePoint enrichment natively
	RemoteHash      string // for normal files, LocalHash == RemoteHash
	LocalSize       int64
	LocalSizeKnown  bool
	RemoteSize      int64
	RemoteSizeKnown bool
	LocalMtime      int64 // local mtime at sync time (Unix nanoseconds)
	RemoteMtime     int64 // remote mtime at sync time; zero means unknown
	// LocalCtime is the local inode change time at sync time (Unix
	// nanoseconds); zero means unknown. The hash fast path requires it
	// unchanged, because an in-place edit can restore the old mtime but
	// never the old ctime (R-2.2.4).
	LocalCtime       int64
	LocalDevice      uint64
	LocalInode       uint64
	LocalHasIdentity bool
	ETag             string
}

// DirNameKey groups baseline entries by (directory, OneDrive namespace key of
// the name) for O(1) sibling lookups under OneDrive's name equivalence. Used
// by detectCaseCollisions to find collisions between new files and
// already-synced baseline entries.
type DirNameKey struct {
	Dir     string
	NameKey string
}

// dirNameKeyFromPath computes the namespace grouping key for a path.
func dirNameKeyFromPath(path string) DirNameKey {
	return DirNameKey{
		Dir:     filepath.Dir(path),
		NameKey: namekey.Key(filepath.Base(path)),
	}
}

// Baseline is the in-memory container for all baseline entries, providing
// triple-key access: by path (primary), by item ID (for move detection),
// and by (directory, namespace key) for case collision detection.
// Maps remain public for test setup convenience; production code MUST use
// the locked accessor methods (GetByPath, GetByID, GetCaseVariants, Put,
// Delete, Len, ForEachPath) which hold mu during access.
type Baseline struct {
	// mu guards the three indexes below, which are always mutated together so
	// a lookup never sees a path present in one index and absent from another.
	mu           sync.RWMutex
	ByPath       map[string]*BaselineEntry
	ByID         map[string]*BaselineEntry       // keyed by item_id within this per-mount DB
	ByDirNameKey map[DirNameKey][]*BaselineEntry // OneDrive-equivalent sibling index (namekey)
}

// GetByPath returns the baseline entry for the given relative path.
// Thread-safe: holds a read lock during access. The returned pointer must not
// be mutated by the caller; mutations outside the lock are not thread-safe.
func (b *Baseline) GetByPath(path string) (*BaselineEntry, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	entry, ok := b.ByPath[path]

	return entry, ok
}

// GetByID returns the baseline entry for the given item ID.
// Thread-safe: holds a read lock during access. The returned pointer must not
// be mutated by the caller; mutations outside the lock are not thread-safe.
func (b *Baseline) GetByID(itemID string) (*BaselineEntry, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	entry, ok := b.ByID[itemID]

	return entry, ok
}

// GetCaseVariants returns all baseline entries in the same directory whose
// name OneDrive treats as the same name (namekey: case, Unicode form,
// ignored emoji). Used by detectCaseCollisions to find
// collisions between new files and already-synced baseline entries.
// The caller must filter out exact path matches (same casing = not a collision).
// Thread-safe: holds a read lock during access.
func (b *Baseline) GetCaseVariants(dir, name string) []*BaselineEntry {
	b.mu.RLock()
	defer b.mu.RUnlock()

	return b.ByDirNameKey[DirNameKey{Dir: dir, NameKey: namekey.Key(name)}]
}

// Put inserts or updates a baseline entry in both maps. If the path already
// exists with a different item ID, the stale ByID entry is removed
// first to prevent orphaned entries (e.g., server-side delete+recreate
// assigns a new item_id for the same path).
// Thread-safe: holds a write lock during access.
func (b *Baseline) Put(entry *BaselineEntry) {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Remove stale ByID entry if the path is being reassigned to a new ID.
	if old, ok := b.ByPath[entry.Path]; ok {
		if old.ItemID != entry.ItemID {
			delete(b.ByID, old.ItemID)
		}
	}
	// The same item at a new path leaves its old path, exactly as the
	// item_id-keyed SQL row does; otherwise the cache kept a ghost entry at
	// the old path.
	if old, ok := b.ByID[entry.ItemID]; ok && old.Path != entry.Path {
		b.deleteLocked(old.Path)
	}

	b.ByPath[entry.Path] = entry
	b.ByID[entry.ItemID] = entry

	// Maintain ByDirNameKey index: update existing entry or append new one.
	dnk := dirNameKeyFromPath(entry.Path)
	found := false

	for i, e := range b.ByDirNameKey[dnk] {
		if e.Path == entry.Path {
			b.ByDirNameKey[dnk][i] = entry
			found = true

			break
		}
	}

	if !found {
		b.ByDirNameKey[dnk] = append(b.ByDirNameKey[dnk], entry)
	}
}

// Delete removes a baseline entry from all three maps by path.
// Thread-safe: holds a write lock during access.
func (b *Baseline) Delete(path string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.deleteLocked(path)
}

// deleteLocked is Delete for a caller holding mu.
func (b *Baseline) deleteLocked(path string) {
	if entry, ok := b.ByPath[path]; ok {
		delete(b.ByID, entry.ItemID)
	}

	delete(b.ByPath, path)

	// Maintain ByDirNameKey index: remove the entry for this exact path.
	dnk := dirNameKeyFromPath(path)
	entries := b.ByDirNameKey[dnk]

	for i, e := range entries {
		if e.Path == path {
			b.ByDirNameKey[dnk] = append(entries[:i], entries[i+1:]...)
			if len(b.ByDirNameKey[dnk]) == 0 {
				delete(b.ByDirNameKey, dnk)
			}

			break
		}
	}
}

// replaceContents swaps in other's indexes so every holder of b sees them.
// other must not be used afterwards.
func (b *Baseline) replaceContents(other *Baseline) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.ByPath = other.ByPath
	b.ByID = other.ByID
	b.ByDirNameKey = other.ByDirNameKey
}

// Len returns the number of entries in the baseline.
// Thread-safe: holds a read lock during access.
func (b *Baseline) Len() int {
	b.mu.RLock()
	defer b.mu.RUnlock()

	return len(b.ByPath)
}

// ForEachPath calls fn for every (path, entry) pair in the baseline.
// The read lock is held for the entire iteration — fn must not call
// any Baseline methods (deadlock). Suitable for read-only observers.
func (b *Baseline) ForEachPath(fn func(string, *BaselineEntry)) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	for path, entry := range b.ByPath {
		fn(path, entry)
	}
}

// FindOrphans identifies baseline entries that are not present in the seen
// set (a full delta enumeration). These represent items deleted remotely but
// missed by incremental delta — deletions are delivered exactly once in a
// narrow window and permanently lost if the client's token advances past them.
//
// When pathPrefix is non-empty, only entries under that prefix are considered
// (used for mount-root observation). When empty, all entries for the given
// driveID are checked.
//
// Returns synthesized ChangeDelete events for each orphan, which can be fed
// through the normal planner + executor pipeline.
func (b *Baseline) FindOrphans(seen map[string]struct{}, driveID driveid.ID, pathPrefix string) []changeEvent {
	var orphans []changeEvent

	b.mu.RLock()
	defer b.mu.RUnlock()

	for p, entry := range b.ByPath {
		if entry.DriveID != driveID {
			continue
		}

		if pathPrefix != "" && p != pathPrefix && !strings.HasPrefix(p, pathPrefix+"/") {
			continue
		}

		if _, ok := seen[entry.ItemID]; ok {
			continue
		}

		orphans = append(orphans, changeEvent{
			Source:    sourceRemote,
			Type:      ChangeDelete,
			Path:      entry.Path,
			ItemID:    entry.ItemID,
			DriveID:   entry.DriveID,
			ItemType:  entry.ItemType,
			IsDeleted: true,
		})
	}

	return orphans
}

// NewBaselineForTest creates a Baseline pre-populated with entries.
// Exported for test files; not intended for production use.
func NewBaselineForTest(entries []*BaselineEntry) *Baseline {
	bl := &Baseline{
		ByPath:       make(map[string]*BaselineEntry, len(entries)),
		ByID:         make(map[string]*BaselineEntry, len(entries)),
		ByDirNameKey: make(map[DirNameKey][]*BaselineEntry, len(entries)),
	}

	for _, e := range entries {
		bl.ByPath[e.Path] = e
		bl.ByID[e.ItemID] = e

		dnk := dirNameKeyFromPath(e.Path)
		bl.ByDirNameKey[dnk] = append(bl.ByDirNameKey[dnk], e)
	}

	return bl
}

// remoteState captures the current state of a path as observed from
// the Graph API delta response.
type remoteState struct {
	ItemID   string
	DriveID  driveid.ID // normalized (lowercase, zero-padded to 16 chars)
	Name     string
	ItemType ItemType
	Size     int64
	// SizeKnown is true when Size is the item's real size (zero included)
	// rather than an absent value; downloads verify against it.
	SizeKnown bool
	Hash      string
	Mtime     int64
	ETag      string
	CTag      string
	IsDeleted bool
}

// localState captures the current state of a path as observed from
// the local filesystem.
type localState struct {
	Name             string
	ItemType         ItemType
	Size             int64
	Hash             string
	Mtime            int64
	LocalCtime       int64 // inode change time (files only); zero means unknown
	LocalDevice      uint64
	LocalInode       uint64
	LocalHasIdentity bool
}

// truthAvailability records whether one side of current path truth is safe for
// the planner to interpret structurally.
type truthAvailability string

const (
	truthAvailabilityAvailable               truthAvailability = "available"
	truthAvailabilityBlockedObservationIssue truthAvailability = "blocked_observation_issue"
)

// pathTruthSource captures which durable authority proved that one side of
// truth is currently unavailable to the planner.
type pathTruthSource string

const (
	pathTruthSourceNone             pathTruthSource = ""
	pathTruthSourceObservationIssue pathTruthSource = "observation_issue"
)

// pathTruthSideStatus is the raw current-truth availability status for one
// side of a path. The zero value is intentionally healthy so callers can use
// it as a derived read-side value without manual initialization.
type pathTruthSideStatus struct {
	Availability truthAvailability
	Source       pathTruthSource
	IssueType    string
	ScopeKey     ScopeKey
}

func (s pathTruthSideStatus) IsAvailable() bool {
	return s.Availability == "" || s.Availability == truthAvailabilityAvailable
}

// pathTruthStatus carries the raw current-truth availability status for both
// the local and remote side of one path. Planner attaches it after SQLite
// computes structural diff so structural absence stays distinct from
// unavailable truth, but the type itself is not planner-owned policy.
type pathTruthStatus struct {
	Local  pathTruthSideStatus
	Remote pathTruthSideStatus
}

func (s *pathTruthStatus) SuppressesStructuralActions() bool {
	if s == nil {
		return false
	}

	return !s.Local.IsAvailable() || !s.Remote.IsAvailable()
}

// pathView is a unified three-way view of a single path built from the
// authoritative snapshots plus baseline. It is the in-memory planning view the
// current actionable-set builder reasons over after SQLite computes structural
// diff and reconciliation outcomes.
type pathView struct {
	Path        string
	Remote      *remoteState   // nil = absent from current remote snapshot
	Local       *localState    // nil = absent from current local snapshot
	Baseline    *BaselineEntry // nil = never synced
	TruthStatus pathTruthStatus
}

// remoteContentDiffersFromBaseline reports whether remote content facts prove
// the item's content changed since the baseline. eTag and mtime churn alone
// (a Graph move rewrites both) are not content changes.
func remoteContentDiffersFromBaseline(remote *remoteState, baseline *BaselineEntry) bool {
	if remote == nil || baseline == nil || baseline.ItemType != ItemTypeFile {
		return false
	}
	if remote.Hash != "" && baseline.RemoteHash != "" && remote.Hash != baseline.RemoteHash {
		return true
	}

	return baseline.RemoteSizeKnown && remote.Size != baseline.RemoteSize
}

// localFileDiffersFromBaseline reports whether a local file's content facts
// show it changed since the baseline: the hash when either side has one,
// otherwise size, then modification time.
func localFileDiffersFromBaseline(local *localState, baseline *BaselineEntry) bool {
	if local == nil || baseline == nil {
		return false
	}
	if baseline.ItemType != ItemTypeFile || local.ItemType != ItemTypeFile {
		return false
	}
	if baseline.LocalHash != "" || local.Hash != "" {
		return baseline.LocalHash != local.Hash
	}
	if baseline.LocalSizeKnown && baseline.LocalSize != local.Size {
		return true
	}

	return baseline.LocalMtime != local.Mtime
}
