package sync

import (
	"path/filepath"
	"strings"
	"sync"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// SkippedItem records a local filesystem entry that was rejected at
// observation time. The scanner collects these alongside events so the
// engine can persist them as durable observation issues.
type SkippedItem struct {
	Path     string // NFC-normalized, relative to sync root
	Reason   string // issue type constant (IssueInvalidFilename, etc.)
	Detail   string // human-readable explanation
	FileSize int64  // populated for IssueFileTooLarge (after stat)
}

// ScanResult is the return type of FullScan. Rows are the direct current local
// snapshot that local_state persists. Events remain observation-local signals
// for watch dirtiness and diagnostics; they are not planner input. Skipped are
// user-actionable rejections (invalid names, path too long, file too large)
// that the engine should record.
type ScanResult struct {
	Events  []ChangeEvent
	Rows    []LocalStateRow
	Skipped []SkippedItem
}

// ChangeEvent is an immutable observation fact produced by observers. The
// runtime uses these facts to mark dirty paths/scopes and to describe local or
// remote observation results, but they are not the durable sync truth and are
// never stored in the database.
//
// Per-observer field contract:
//
//	RemoteObserver populates: all fields (ItemID, ParentID, DriveID, ETag,
//	  CTag, Hash, Size, Mtime). For ChangeDelete: Hash is empty; Size/Mtime
//	  from API response.
//	LocalObserver populates: Path, Name, ItemType, Size, Hash, Mtime,
//	  IsDeleted. Never sets: ItemID, ParentID, DriveID, ETag, CTag.
//	  For ChangeDelete: Hash is empty; Size/Mtime from baseline entry.
type ChangeEvent struct {
	Source           ChangeSource
	Type             ChangeType
	Path             string     // NFC-normalized, relative to sync root
	OldPath          string     // for moves only
	ItemID           string     // server-assigned (remote only; empty for local)
	ParentID         string     // server parent ID (remote only)
	DriveID          driveid.ID // normalized (lowercase, zero-padded to 16 chars)
	ItemType         ItemType
	Name             string // URL-decoded, NFC-normalized
	Size             int64
	Hash             string // QuickXorHash (base64); empty for folders
	Mtime            int64  // Unix nanoseconds
	ETag             string // remote only
	CTag             string // remote only
	IsDeleted        bool
	TargetRootItemID string // configured remote root item for shared-root observation
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
	ETag            string
}

// DirLowerKey groups baseline entries by (directory, lowercase name) for
// O(1) case-insensitive sibling lookups. Used by detectCaseCollisions to
// find collisions between new files and already-synced baseline entries.
type DirLowerKey struct {
	Dir     string
	LowName string
}

// DirLowerKeyFromPath computes the case-insensitive grouping key for a path.
func DirLowerKeyFromPath(path string) DirLowerKey {
	return DirLowerKey{
		Dir:     filepath.Dir(path),
		LowName: strings.ToLower(filepath.Base(path)),
	}
}

// Baseline is the in-memory container for all baseline entries, providing
// triple-key access: by path (primary), by item ID (for move detection),
// and by (directory, lowercase name) for case collision detection.
// Maps remain public for test setup convenience; production code MUST use
// the locked accessor methods (GetByPath, GetByID, GetCaseVariants, Put,
// Delete, Len, ForEachPath) which hold mu during access.
type Baseline struct {
	mu         sync.RWMutex
	ByPath     map[string]*BaselineEntry
	ByID       map[string]*BaselineEntry        // keyed by item_id within this per-drive DB
	ByDirLower map[DirLowerKey][]*BaselineEntry // case-insensitive sibling index
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
// name matches case-insensitively. Used by detectCaseCollisions to find
// collisions between new files and already-synced baseline entries.
// The caller must filter out exact path matches (same casing = not a collision).
// Thread-safe: holds a read lock during access.
func (b *Baseline) GetCaseVariants(dir, name string) []*BaselineEntry {
	b.mu.RLock()
	defer b.mu.RUnlock()

	return b.ByDirLower[DirLowerKey{Dir: dir, LowName: strings.ToLower(name)}]
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

	b.ByPath[entry.Path] = entry
	b.ByID[entry.ItemID] = entry

	// Maintain ByDirLower index: update existing entry or append new one.
	dlk := DirLowerKeyFromPath(entry.Path)
	found := false

	for i, e := range b.ByDirLower[dlk] {
		if e.Path == entry.Path {
			b.ByDirLower[dlk][i] = entry
			found = true

			break
		}
	}

	if !found {
		b.ByDirLower[dlk] = append(b.ByDirLower[dlk], entry)
	}
}

// Delete removes a baseline entry from all three maps by path.
// Thread-safe: holds a write lock during access.
func (b *Baseline) Delete(path string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if entry, ok := b.ByPath[path]; ok {
		delete(b.ByID, entry.ItemID)
	}

	delete(b.ByPath, path)

	// Maintain ByDirLower index: remove the entry for this exact path.
	dlk := DirLowerKeyFromPath(path)
	entries := b.ByDirLower[dlk]

	for i, e := range entries {
		if e.Path == path {
			b.ByDirLower[dlk] = append(entries[:i], entries[i+1:]...)
			if len(b.ByDirLower[dlk]) == 0 {
				delete(b.ByDirLower, dlk)
			}

			break
		}
	}
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

// DescendantsOf returns all baseline entries whose path is strictly under
// the given prefix (prefix + "/"). The prefix itself is excluded. Used by
// the planner's folder delete cascade expansion to synthesize child delete
// actions when the delta API only reports the parent folder as deleted.
// Thread-safe: holds a read lock during access.
func (b *Baseline) DescendantsOf(prefix string) []*BaselineEntry {
	b.mu.RLock()
	defer b.mu.RUnlock()

	childPrefix := prefix + "/"
	var descendants []*BaselineEntry

	for p, entry := range b.ByPath {
		if strings.HasPrefix(p, childPrefix) {
			descendants = append(descendants, entry)
		}
	}

	return descendants
}

// FindOrphans identifies baseline entries that are not present in the seen
// set (a full delta enumeration). These represent items deleted remotely but
// missed by incremental delta — deletions are delivered exactly once in a
// narrow window and permanently lost if the client's token advances past them.
//
// When pathPrefix is non-empty, only entries under that prefix are considered
// (used for shared-root observation). When empty, all entries for the given
// driveID are checked.
//
// Returns synthesized ChangeDelete events for each orphan, which can be fed
// through the normal planner + executor pipeline.
func (b *Baseline) FindOrphans(seen map[string]struct{}, driveID driveid.ID, pathPrefix string) []ChangeEvent {
	var orphans []ChangeEvent

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

		orphans = append(orphans, ChangeEvent{
			Source:    SourceRemote,
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
		ByPath:     make(map[string]*BaselineEntry, len(entries)),
		ByID:       make(map[string]*BaselineEntry, len(entries)),
		ByDirLower: make(map[DirLowerKey][]*BaselineEntry, len(entries)),
	}

	for _, e := range entries {
		bl.ByPath[e.Path] = e
		bl.ByID[e.ItemID] = e

		dlk := DirLowerKeyFromPath(e.Path)
		bl.ByDirLower[dlk] = append(bl.ByDirLower[dlk], e)
	}

	return bl
}

// RemoteState captures the current state of a path as observed from
// the Graph API delta response.
type RemoteState struct {
	ItemID           string
	DriveID          driveid.ID // normalized (lowercase, zero-padded to 16 chars)
	ParentID         string
	Name             string
	ItemType         ItemType
	Size             int64
	Hash             string
	Mtime            int64
	ETag             string
	CTag             string
	IsDeleted        bool
	TargetRootItemID string
}

// LocalState captures the current state of a path as observed from
// the local filesystem.
type LocalState struct {
	Name     string
	ItemType ItemType
	Size     int64
	Hash     string
	Mtime    int64
}

// PathView is a unified three-way view of a single path built from the
// authoritative snapshots plus baseline. It is the in-memory planning view the
// current actionable-set builder reasons over after SQLite computes structural
// diff and reconciliation outcomes.
type PathView struct {
	Path                      string
	Remote                    *RemoteState   // nil = absent from current remote snapshot
	Local                     *LocalState    // nil = absent from current local snapshot
	Baseline                  *BaselineEntry // nil = never synced
	ObservationIssueType      string
	ObservationIssueScopeKey  ScopeKey
	LocalReadScope            ScopeKey
	RemoteReadScope           ScopeKey
	LocalAvailabilityBlocked  bool
	RemoteAvailabilityBlocked bool
}

// ConflictRecord holds metadata about a detected conflict.
type ConflictRecord struct {
	ID           string
	DriveID      driveid.ID
	ItemID       string
	Path         string
	Name         string // derived: path.Base(Path), for display convenience (B-071)
	ConflictType string // ConflictEditEdit, ConflictEditDelete, ConflictCreateCreate
	DetectedAt   int64
	LocalHash    string
	RemoteHash   string
	LocalMtime   int64
	RemoteMtime  int64
	Resolution   string // final outcome for internal bookkeeping
	ResolvedAt   int64  // 0 if unresolved
	ResolvedBy   string // ResolvedByAuto or "" if unresolved
}

// VerifyResult describes the verification status of a single file.
type VerifyResult struct {
	Path     string `json:"path"`
	Status   string `json:"status"` // "ok", "missing", "hash_mismatch", "size_mismatch"
	Expected string `json:"expected,omitempty"`
	Actual   string `json:"actual,omitempty"`
}

// VerifyReport summarizes a full-tree hash verification of local files
// against the baseline database.
type VerifyReport struct {
	Verified   int            `json:"verified"`
	Mismatches []VerifyResult `json:"mismatches"`
}
