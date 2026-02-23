// Package sync implements the event-driven sync engine for bidirectional
// OneDrive synchronization. This file defines all types used across the
// sync pipeline: enums, events, baseline state, planner views, actions,
// and outcomes.
package sync

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// ---------------------------------------------------------------------------
// Enums
// ---------------------------------------------------------------------------

// String constants for enum serialization (shared by String() and Parse*).
const (
	strRemote = "remote"
	strLocal  = "local"
	strFile   = "file"
	strFolder = "folder"
	strRoot   = "root"
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
		return "download"
	case ActionUpload:
		return "upload"
	case ActionLocalDelete:
		return "local_delete"
	case ActionRemoteDelete:
		return "remote_delete"
	case ActionLocalMove:
		return "local_move"
	case ActionRemoteMove:
		return "remote_move"
	case ActionFolderCreate:
		return "folder_create"
	case ActionConflict:
		return "conflict"
	case ActionUpdateSynced:
		return "update_synced"
	case ActionCleanup:
		return "cleanup"
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
	Path      string // NFC-normalized, relative to sync root
	OldPath   string // for moves only
	ItemID    string // server-assigned (remote only; empty for local)
	ParentID  string // server parent ID (remote only)
	DriveID   string // normalized (lowercase, zero-padded to 16 chars)
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
	DriveID    string
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
type Baseline struct {
	ByPath map[string]*BaselineEntry
	ByID   map[string]*BaselineEntry // keyed by "driveID:itemID"
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
	DriveID   string // normalized (lowercase, zero-padded to 16 chars)
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
	DriveID      string
	ItemID       string
	Path         string
	ConflictType string // "edit_edit", "edit_delete", "create_create"
	DetectedAt   int64
	LocalHash    string
	RemoteHash   string
	LocalMtime   int64
	RemoteMtime  int64
}

// Action is an instruction for the executor, produced by the planner.
type Action struct {
	Type         ActionType
	DriveID      string
	ItemID       string
	Path         string
	NewPath      string           // for moves
	CreateSide   FolderCreateSide // for folder creates
	View         *PathView        // full three-way context
	ConflictInfo *ConflictRecord
}

// ActionPlan contains 9 ordered slices of actions, executed in sequence
// by the executor. The ordering ensures correctness (e.g., folder creates
// before file downloads, depth-first for deletes).
type ActionPlan struct {
	FolderCreates []Action
	Moves         []Action
	Downloads     []Action
	Uploads       []Action
	LocalDeletes  []Action
	RemoteDeletes []Action
	Conflicts     []Action
	SyncedUpdates []Action
	Cleanups      []Action
}

// Outcome is the result of executing a single action. Self-contained —
// has everything the BaselineManager needs to update the database.
type Outcome struct {
	Action       ActionType
	Success      bool
	Error        error
	Path         string
	OldPath      string // for moves
	DriveID      string
	ItemID       string // from API response after upload
	ParentID     string
	ItemType     ItemType
	LocalHash    string
	RemoteHash   string
	Size         int64
	Mtime        int64 // local mtime at sync time
	ETag         string
	ConflictType string // "edit_edit", "edit_delete", "create_create" (conflicts only)
}

// ---------------------------------------------------------------------------
// Consumer-defined interfaces (satisfied by *graph.Client)
// ---------------------------------------------------------------------------

// DeltaFetcher fetches a page of delta changes from the Graph API.
type DeltaFetcher interface {
	Delta(ctx context.Context, driveID, token string) (*graph.DeltaPage, error)
}

// ItemClient provides CRUD operations on drive items.
type ItemClient interface {
	GetItem(ctx context.Context, driveID, itemID string) (*graph.Item, error)
	ListChildren(ctx context.Context, driveID, parentID string) ([]graph.Item, error)
	CreateFolder(ctx context.Context, driveID, parentID, name string) (*graph.Item, error)
	MoveItem(ctx context.Context, driveID, itemID, newParentID, newName string) (*graph.Item, error)
	DeleteItem(ctx context.Context, driveID, itemID string) error
}

// TransferClient provides file transfer operations.
type TransferClient interface {
	Download(ctx context.Context, driveID, itemID string, w io.Writer) (int64, error)
	SimpleUpload(ctx context.Context, driveID, parentID, name string, r io.Reader, size int64) (*graph.Item, error)
	CreateUploadSession(ctx context.Context, driveID, parentID, name string, size int64, mtime time.Time) (*graph.UploadSession, error)
	UploadChunk(ctx context.Context, session *graph.UploadSession, chunk io.Reader, offset, length, total int64) (*graph.Item, error)
}
