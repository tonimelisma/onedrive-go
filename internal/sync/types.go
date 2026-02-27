// Package sync implements the event-driven sync engine for bidirectional
// OneDrive synchronization. This file defines all types used across the
// sync pipeline: enums, events, baseline state, planner views, actions,
// and outcomes.
package sync

import (
	"context"
	"fmt"
	"io"
	stdsync "sync"
	"time"

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
	Source    ChangeSource
	Type      ChangeType
	Path      string     // NFC-normalized, relative to sync root
	OldPath   string     // for moves only
	ItemID    string     // server-assigned (remote only; empty for local)
	ParentID  string     // server parent ID (remote only)
	DriveID   driveid.ID // normalized (lowercase, zero-padded to 16 chars)
	ItemType  ItemType
	Name      string // URL-decoded, NFC-normalized
	Size      int64
	Hash      string // QuickXorHash (base64); empty for folders
	Mtime     int64  // Unix nanoseconds
	ETag      string // remote only
	CTag      string // remote only
	IsDeleted bool
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
	ByPath map[string]*BaselineEntry
	ByID   map[driveid.ItemKey]*BaselineEntry // keyed by (driveID, itemID) pair
}

// GetByPath returns the baseline entry for the given relative path.
// Thread-safe: holds a read lock during access.
func (b *Baseline) GetByPath(path string) (*BaselineEntry, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	entry, ok := b.ByPath[path]

	return entry, ok
}

// GetByID returns the baseline entry for the given (driveID, itemID) pair.
// Thread-safe: holds a read lock during access.
func (b *Baseline) GetByID(key driveid.ItemKey) (*BaselineEntry, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	entry, ok := b.ByID[key]

	return entry, ok
}

// Put inserts or updates a baseline entry in both maps.
// Thread-safe: holds a write lock during access.
func (b *Baseline) Put(entry *BaselineEntry) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.ByPath[entry.Path] = entry
	b.ByID[driveid.NewItemKey(entry.DriveID, entry.ItemID)] = entry
}

// Delete removes a baseline entry from both maps by path.
// Thread-safe: holds a write lock during access.
func (b *Baseline) Delete(path string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if entry, ok := b.ByPath[path]; ok {
		delete(b.ByID, driveid.NewItemKey(entry.DriveID, entry.ItemID))
	}

	delete(b.ByPath, path)
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
	Mismatches []VerifyResult `json:"mismatches,omitempty"`
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
	CycleID string   // UUID grouping actions from one planning pass
}

// Outcome is the result of executing a single action. Self-contained —
// has everything the BaselineManager needs to update the database.
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

// Downloader streams a remote file by item ID.
type Downloader interface {
	Download(ctx context.Context, driveID driveid.ID, itemID string, w io.Writer) (int64, error)
}

// Uploader uploads a local file, encapsulating the simple-vs-chunked decision
// and upload session lifecycle. content must be an io.ReaderAt for retry safety.
type Uploader interface {
	Upload(
		ctx context.Context, driveID driveid.ID, parentID, name string,
		content io.ReaderAt, size int64, mtime time.Time, progress graph.ProgressFunc,
	) (*graph.Item, error)
}
