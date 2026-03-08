// Package sync implements the event-driven sync engine for bidirectional
// OneDrive synchronization. This file defines all types used across the
// sync pipeline: enums, events, baseline state, planner views, actions,
// and outcomes.
package sync

import (
	"context"
	"fmt"
	"strings"
	stdsync "sync"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// ---------------------------------------------------------------------------
// Enums
// ---------------------------------------------------------------------------

// String constants for enum serialization (shared by String() and Parse*).
const (
	strRemote       = "remote"
	strLocal        = "local"
	strFile         = "file"
	strFolder       = "folder"
	strRoot         = "root"
	strDownload     = "download"
	strUpload       = "upload"
	strDelete       = "delete"
	strActionable   = "actionable"
	strTransient    = "transient"
	strLocalDelete  = "local_delete"
	strRemoteDelete = "remote_delete"
	strLocalMove    = "local_move"
	strRemoteMove   = "remote_move"
	strFolderCreate = "folder_create"
	strConflict     = "conflict"
	strUpdateSynced = "update_synced"
	strCleanup      = "cleanup"
)

// Resolution strategy constants for conflict resolution.
const (
	ResolutionKeepLocal  = "keep_local"
	ResolutionKeepRemote = "keep_remote"
	ResolutionKeepBoth   = "keep_both"
	ResolutionUnresolved = "unresolved"
)

// Conflict type constants.
const (
	ConflictEditEdit     = "edit_edit"
	ConflictEditDelete   = "edit_delete"
	ConflictCreateCreate = "create_create"
)

// ResolvedBy constants for conflict resolution attribution.
const (
	ResolvedByAuto = "auto"
	ResolvedByUser = "user"
)

// ChangeSource identifies the origin of a change event.
type ChangeSource int

const (
	// SourceRemote indicates the change was observed from the Graph API.
	SourceRemote ChangeSource = iota
	// SourceLocal indicates the change was observed from the local filesystem.
	SourceLocal
)

func (s ChangeSource) String() string {
	switch s {
	case SourceRemote:
		return strRemote
	case SourceLocal:
		return strLocal
	default:
		return fmt.Sprintf("ChangeSource(%d)", int(s))
	}
}

// ChangeType classifies what kind of change occurred.
type ChangeType int

const (
	ChangeCreate ChangeType = iota
	ChangeModify
	ChangeDelete
	ChangeMove
	ChangeShortcut // shortcut/shared folder detected (remote only)
)

func (t ChangeType) String() string {
	switch t {
	case ChangeCreate:
		return "create"
	case ChangeModify:
		return "modify"
	case ChangeDelete:
		return "delete"
	case ChangeMove:
		return "move"
	case ChangeShortcut:
		return "shortcut"
	default:
		return fmt.Sprintf("ChangeType(%d)", int(t))
	}
}

// ItemType classifies the kind of item (file, folder, or drive root).
// Stored as TEXT in SQLite ("file"/"folder"/"root").
type ItemType int

const (
	ItemTypeFile ItemType = iota
	ItemTypeFolder
	ItemTypeRoot
)

func (t ItemType) String() string {
	switch t {
	case ItemTypeFile:
		return strFile
	case ItemTypeFolder:
		return strFolder
	case ItemTypeRoot:
		return strRoot
	default:
		return fmt.Sprintf("ItemType(%d)", int(t))
	}
}

// ParseItemType converts a database TEXT value to ItemType.
func ParseItemType(s string) (ItemType, error) {
	switch s {
	case strFile:
		return ItemTypeFile, nil
	case strFolder:
		return ItemTypeFolder, nil
	case strRoot:
		return ItemTypeRoot, nil
	default:
		return ItemTypeFile, fmt.Errorf("sync: unknown item type %q", s)
	}
}

// SyncMode controls the directionality of synchronization.
type SyncMode int

const (
	SyncBidirectional SyncMode = iota
	SyncDownloadOnly
	SyncUploadOnly
)

func (m SyncMode) String() string {
	switch m {
	case SyncBidirectional:
		return "bidirectional"
	case SyncDownloadOnly:
		return "download-only"
	case SyncUploadOnly:
		return "upload-only"
	default:
		return fmt.Sprintf("SyncMode(%d)", int(m))
	}
}

// ActionType classifies what the executor should do for a given action.
type ActionType int

const (
	ActionDownload ActionType = iota
	ActionUpload
	ActionLocalDelete
	ActionRemoteDelete
	ActionLocalMove
	ActionRemoteMove
	ActionFolderCreate
	ActionConflict
	ActionUpdateSynced
	ActionCleanup
)

func (a ActionType) String() string {
	switch a {
	case ActionDownload:
		return strDownload
	case ActionUpload:
		return strUpload
	case ActionLocalDelete:
		return strLocalDelete
	case ActionRemoteDelete:
		return strRemoteDelete
	case ActionLocalMove:
		return strLocalMove
	case ActionRemoteMove:
		return strRemoteMove
	case ActionFolderCreate:
		return strFolderCreate
	case ActionConflict:
		return strConflict
	case ActionUpdateSynced:
		return strUpdateSynced
	case ActionCleanup:
		return strCleanup
	default:
		return fmt.Sprintf("ActionType(%d)", int(a))
	}
}

// FolderCreateSide specifies where a new folder should be created.
type FolderCreateSide int

const (
	CreateLocal FolderCreateSide = iota
	CreateRemote
)

func (s FolderCreateSide) String() string {
	switch s {
	case CreateLocal:
		return strLocal
	case CreateRemote:
		return strRemote
	default:
		return fmt.Sprintf("FolderCreateSide(%d)", int(s))
	}
}

// ---------------------------------------------------------------------------
// Core structs
// ---------------------------------------------------------------------------

// ChangeEvent is an immutable observation of a change, produced by observers
// and consumed by the change buffer and planner. Never stored in the database
// (except optionally in the change journal for debugging).
//
// Per-observer field contract:
//
//	RemoteObserver populates: all fields (ItemID, ParentID, DriveID, ETag,
//	  CTag, Hash, Size, Mtime). For ChangeDelete: Hash is empty; Size/Mtime
//	  from API response.
//	LocalObserver populates: Path, Name, ItemType, Size, Hash, Mtime,
//	  IsDeleted. Never sets: ItemID, ParentID, DriveID, ETag, CTag.
//	  For ChangeDelete: Hash is empty; Size/Mtime from baseline entry.
//	Buffer synthetic deletes: Source, Type, Path, ItemID, ParentID, DriveID,
//	  ItemType, Name, IsDeleted. No Size/Hash/Mtime (move context only).
type ChangeEvent struct {
	Source        ChangeSource
	Type          ChangeType
	Path          string     // NFC-normalized, relative to sync root
	OldPath       string     // for moves only
	ItemID        string     // server-assigned (remote only; empty for local)
	ParentID      string     // server parent ID (remote only)
	DriveID       driveid.ID // normalized (lowercase, zero-padded to 16 chars)
	ItemType      ItemType
	Name          string // URL-decoded, NFC-normalized
	Size          int64
	Hash          string // QuickXorHash (base64); empty for folders
	Mtime         int64  // Unix nanoseconds
	ETag          string // remote only
	CTag          string // remote only
	IsDeleted     bool
	RemoteDriveID string // for shortcuts: source drive containing shared content
	RemoteItemID  string // for shortcuts: source folder ID on the remote drive
}

// BaselineEntry represents the confirmed synced state of a single path.
// This is the ONLY durable per-item state in the system.
type BaselineEntry struct {
	Path       string
	DriveID    driveid.ID
	ItemID     string
	ParentID   string
	ItemType   ItemType
	LocalHash  string // per-side: handles SharePoint enrichment natively
	RemoteHash string // for normal files, LocalHash == RemoteHash
	Size       int64
	Mtime      int64 // local mtime at sync time (Unix nanoseconds)
	SyncedAt   int64 // when this entry was last confirmed synced
	ETag       string
}

// Baseline is the in-memory container for all baseline entries, providing
// dual-key access by path (primary) and by item ID (for move detection).
// Maps remain public for test setup convenience; production code MUST use
// the locked accessor methods (GetByPath, GetByID, Put, Delete, Len,
// ForEachPath) which hold mu during access.
type Baseline struct {
	mu     stdsync.RWMutex
	byPath map[string]*BaselineEntry
	byID   map[driveid.ItemKey]*BaselineEntry // keyed by (driveID, itemID) pair
}

// GetByPath returns the baseline entry for the given relative path.
// Thread-safe: holds a read lock during access. The returned pointer must not
// be mutated by the caller; mutations outside the lock are not thread-safe.
func (b *Baseline) GetByPath(path string) (*BaselineEntry, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	entry, ok := b.byPath[path]

	return entry, ok
}

// GetByID returns the baseline entry for the given (driveID, itemID) pair.
// Thread-safe: holds a read lock during access. The returned pointer must not
// be mutated by the caller; mutations outside the lock are not thread-safe.
func (b *Baseline) GetByID(key driveid.ItemKey) (*BaselineEntry, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	entry, ok := b.byID[key]

	return entry, ok
}

// Put inserts or updates a baseline entry in both maps. If the path already
// exists with a different (driveID, itemID), the stale ByID entry is removed
// first to prevent orphaned entries (e.g., server-side delete+recreate
// assigns a new item_id for the same path).
// Thread-safe: holds a write lock during access.
func (b *Baseline) Put(entry *BaselineEntry) {
	b.mu.Lock()
	defer b.mu.Unlock()

	newKey := driveid.NewItemKey(entry.DriveID, entry.ItemID)

	// Remove stale ByID entry if the path is being reassigned to a new ID.
	if old, ok := b.byPath[entry.Path]; ok {
		oldKey := driveid.NewItemKey(old.DriveID, old.ItemID)
		if oldKey != newKey {
			delete(b.byID, oldKey)
		}
	}

	b.byPath[entry.Path] = entry
	b.byID[newKey] = entry
}

// Delete removes a baseline entry from both maps by path.
// Thread-safe: holds a write lock during access.
func (b *Baseline) Delete(path string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if entry, ok := b.byPath[path]; ok {
		delete(b.byID, driveid.NewItemKey(entry.DriveID, entry.ItemID))
	}

	delete(b.byPath, path)
}

// Len returns the number of entries in the baseline.
// Thread-safe: holds a read lock during access.
func (b *Baseline) Len() int {
	b.mu.RLock()
	defer b.mu.RUnlock()

	return len(b.byPath)
}

// ForEachPath calls fn for every (path, entry) pair in the baseline.
// The read lock is held for the entire iteration — fn must not call
// any Baseline methods (deadlock). Suitable for read-only observers.
func (b *Baseline) ForEachPath(fn func(string, *BaselineEntry)) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	for path, entry := range b.byPath {
		fn(path, entry)
	}
}

// FindOrphans identifies baseline entries that are not present in the seen
// set (a full delta enumeration). These represent items deleted remotely but
// missed by incremental delta — deletions are delivered exactly once in a
// narrow window and permanently lost if the client's token advances past them.
//
// When pathPrefix is non-empty, only entries under that prefix are considered
// (used for shortcut-scoped orphan detection). When empty, all entries for the
// given driveID are checked.
//
// Returns synthesized ChangeDelete events for each orphan, which can be fed
// through the normal planner + executor pipeline.
func (b *Baseline) FindOrphans(seen map[driveid.ItemKey]struct{}, driveID driveid.ID, pathPrefix string) []ChangeEvent {
	var orphans []ChangeEvent

	b.mu.RLock()
	defer b.mu.RUnlock()

	for p, entry := range b.byPath {
		if entry.DriveID != driveID {
			continue
		}

		if pathPrefix != "" && p != pathPrefix && !strings.HasPrefix(p, pathPrefix+"/") {
			continue
		}

		key := driveid.NewItemKey(entry.DriveID, entry.ItemID)
		if _, ok := seen[key]; ok {
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
// Exported for test files within the package; not intended for production use.
func NewBaselineForTest(entries []*BaselineEntry) *Baseline {
	bl := &Baseline{
		byPath: make(map[string]*BaselineEntry, len(entries)),
		byID:   make(map[driveid.ItemKey]*BaselineEntry, len(entries)),
	}

	for _, e := range entries {
		bl.byPath[e.Path] = e
		bl.byID[driveid.NewItemKey(e.DriveID, e.ItemID)] = e
	}

	return bl
}

// PathChanges groups all change events for a single path, separating
// remote and local observations.
type PathChanges struct {
	Path         string
	RemoteEvents []ChangeEvent
	LocalEvents  []ChangeEvent
}

// RemoteState captures the current state of a path as observed from
// the Graph API delta response.
type RemoteState struct {
	ItemID    string
	DriveID   driveid.ID // normalized (lowercase, zero-padded to 16 chars)
	ParentID  string
	Name      string
	ItemType  ItemType
	Size      int64
	Hash      string
	Mtime     int64
	ETag      string
	CTag      string
	IsDeleted bool
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

// PathView is a unified three-way view of a single path. Constructed by
// the planner from change events + baseline. Input to the reconciliation
// decision matrix.
type PathView struct {
	Path     string
	Remote   *RemoteState   // nil = no remote change observed
	Local    *LocalState    // nil = no local change observed / locally absent
	Baseline *BaselineEntry // nil = never synced
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
	Resolution   string // ResolutionUnresolved, ResolutionKeepLocal, ResolutionKeepRemote, ResolutionKeepBoth
	ResolvedAt   int64  // 0 if unresolved
	ResolvedBy   string // ResolvedByUser, ResolvedByAuto, or "" if unresolved
}

// Shortcut represents a OneDrive shortcut or shared folder that requires
// separate observation on the source drive. Stored in the shortcuts table.
// Note: the shortcuts table has a read_only column that is no longer used —
// permission state lives entirely in sync_failures + in-memory permission cache.
type Shortcut struct {
	ItemID       string // shortcut item ID in the user's drive
	RemoteDrive  string // source drive ID
	RemoteItem   string // source folder ID
	LocalPath    string // local filesystem path for this shortcut's content
	DriveType    string // source drive type: "personal", "business", "documentLibrary"
	Observation  string // "unknown", "delta", or "enumerate"
	DiscoveredAt int64  // unix timestamp when first seen
}

// Shortcut observation strategies.
const (
	ObservationUnknown   = "unknown"
	ObservationDelta     = "delta"
	ObservationEnumerate = "enumerate"
)

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

// Action is an instruction for the executor, produced by the planner.
type Action struct {
	Type         ActionType
	DriveID      driveid.ID
	ItemID       string
	Path         string           // canonical path (destination for moves)
	OldPath      string           // source path (moves only)
	CreateSide   FolderCreateSide // for folder creates
	View         *PathView        // full three-way context
	ConflictInfo *ConflictRecord
}

// ActionPlan contains a flat list of actions with explicit dependency edges.
// The Deps adjacency list encodes ordering constraints (parent-before-child,
// children-before-parent-delete, move-target-parent).
type ActionPlan struct {
	Actions []Action // flat list of all actions
	Deps    [][]int  // Deps[i] = indices that action i depends on
}

// Outcome is the result of executing a single action. Self-contained —
// has everything the SyncStore needs to update the database.
type Outcome struct {
	Action       ActionType
	Success      bool
	Error        error
	Path         string
	OldPath      string // for moves
	DriveID      driveid.ID
	ItemID       string // from API response after upload
	ParentID     string
	ItemType     ItemType
	LocalHash    string
	RemoteHash   string
	Size         int64
	Mtime        int64 // local mtime at sync time
	RemoteMtime  int64 // remote mtime for conflict records
	ETag         string
	ConflictType string // ConflictEditDelete etc. (conflicts only)
	ResolvedBy   string // ResolvedByAuto for auto-resolved conflicts, "" otherwise
}

// ---------------------------------------------------------------------------
// Consumer-defined interfaces (satisfied by *graph.Client)
// ---------------------------------------------------------------------------

// DeltaFetcher fetches a page of delta changes from the Graph API.
type DeltaFetcher interface {
	Delta(ctx context.Context, driveID driveid.ID, token string) (*graph.DeltaPage, error)
}

// ItemClient provides CRUD operations on drive items.
type ItemClient interface {
	GetItem(ctx context.Context, driveID driveid.ID, itemID string) (*graph.Item, error)
	ListChildren(ctx context.Context, driveID driveid.ID, parentID string) ([]graph.Item, error)
	CreateFolder(ctx context.Context, driveID driveid.ID, parentID, name string) (*graph.Item, error)
	MoveItem(ctx context.Context, driveID driveid.ID, itemID, newParentID, newName string) (*graph.Item, error)
	DeleteItem(ctx context.Context, driveID driveid.ID, itemID string) error
	PermanentDeleteItem(ctx context.Context, driveID driveid.ID, itemID string) error
}

// DriveVerifier verifies that a configured drive ID is reachable and matches
// the remote API. Used at engine startup to detect stale config (B-074).
type DriveVerifier interface {
	Drive(ctx context.Context, driveID driveid.ID) (*graph.Drive, error)
}

// FolderDeltaFetcher provides folder-scoped delta enumeration for shortcut
// observation on personal drives (6.4b).
type FolderDeltaFetcher interface {
	DeltaFolderAll(ctx context.Context, driveID driveid.ID, folderID, token string) ([]graph.Item, string, error)
}

// RecursiveLister provides recursive children enumeration for shortcut
// observation on business/SharePoint drives where folder-scoped delta
// is not supported (6.4b).
type RecursiveLister interface {
	ListChildrenRecursive(ctx context.Context, driveID driveid.ID, folderID string) ([]graph.Item, error)
}
