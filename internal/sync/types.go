// Package sync implements the bidirectional sync engine for onedrive-go.
// It provides state management, delta processing, local scanning, filtering,
// reconciliation, safety checks, and execution — the full sync pipeline.
package sync

import (
	"context"
	"io"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// ItemType represents the kind of drive item.
type ItemType string

// Item types as stored in the database item_type column.
const (
	ItemTypeFile   ItemType = "file"
	ItemTypeFolder ItemType = "folder"
	ItemTypeRoot   ItemType = "root"
	ItemTypeRemote ItemType = "remote"
)

// Item represents a tracked file or folder in the sync state database.
// It stores three views of each item: remote (from API), local (from filesystem),
// and synced base (snapshot at last successful sync). This three-state model
// enables the three-way merge algorithm (data-model.md section 3).
type Item struct {
	// Identity
	DriveID       string
	ItemID        string
	ParentDriveID string
	ParentID      string
	Name          string
	ItemType      ItemType
	Path          string // materialized local path (relative to sync root)

	// Remote state (from Graph API, updated by delta processor)
	Size         *int64 // nullable: deleted Personal items lack size
	ETag         string
	CTag         string
	QuickXorHash string // base64-encoded QuickXorHash (files only)
	SHA256Hash   string // hex SHA-256 (Business-only, opportunistic)
	RemoteMtime  *int64 // server lastModifiedDateTime as Unix nanoseconds

	// Local state (from filesystem scanner)
	LocalSize  *int64 // last-known local file size in bytes
	LocalMtime *int64 // local modification time as Unix nanoseconds
	LocalHash  string // last-computed local QuickXorHash (base64)

	// Sync base state (snapshot at last successful sync)
	SyncedSize   *int64 // size at last successful sync
	SyncedMtime  *int64 // mtime at last successful sync (Unix nanoseconds)
	SyncedHash   string // hash at last successful sync (base64)
	LastSyncedAt *int64 // timestamp of last sync operation (Unix nanoseconds)

	// Shared/remote item references
	RemoteDriveID string // target drive for shared/remote items
	RemoteID      string // target item ID for shared/remote items

	// Tombstone fields
	IsDeleted bool
	DeletedAt *int64 // tombstone creation timestamp (Unix nanoseconds)

	// Row metadata
	CreatedAt int64 // row creation (Unix nanoseconds)
	UpdatedAt int64 // row last update (Unix nanoseconds)
}

// ConflictResolution describes how a conflict was resolved.
type ConflictResolution string

// Conflict resolution strategies as stored in the conflicts table.
const (
	ConflictUnresolved ConflictResolution = "unresolved"
	ConflictKeepBoth   ConflictResolution = "keep_both"
	ConflictKeepLocal  ConflictResolution = "keep_local"
	ConflictKeepRemote ConflictResolution = "keep_remote"
	ConflictManual     ConflictResolution = "manual"
)

// ConflictResolvedBy indicates who resolved a conflict.
type ConflictResolvedBy string

// Values for the resolved_by column.
const (
	ResolvedByUser ConflictResolvedBy = "user"
	ResolvedByAuto ConflictResolvedBy = "auto"
)

// ConflictRecord represents a file conflict entry in the conflict ledger
// (data-model.md section 5).
type ConflictRecord struct {
	ID          string
	DriveID     string
	ItemID      string
	Path        string // file path at time of conflict detection
	DetectedAt  int64  // Unix nanoseconds
	LocalHash   string
	RemoteHash  string
	LocalMtime  *int64
	RemoteMtime *int64
	Resolution  ConflictResolution
	ResolvedAt  *int64
	ResolvedBy  *ConflictResolvedBy
	History     string // JSON array of resolution events
}

// StaleRecord represents a file that became excluded by filter changes
// but still exists locally (data-model.md section 6).
type StaleRecord struct {
	ID         string
	Path       string
	Reason     string
	DetectedAt int64  // Unix nanoseconds
	Size       *int64 // file size for display
}

// UploadSessionRecord represents a resumable upload session tracked in the
// database for crash recovery (data-model.md section 7).
type UploadSessionRecord struct {
	ID            string
	DriveID       string
	ItemID        string // empty for new file uploads
	LocalPath     string
	SessionURL    string // pre-authenticated upload URL
	Expiry        int64  // session expiration (Unix nanoseconds)
	BytesUploaded int64
	TotalSize     int64
	CreatedAt     int64 // Unix nanoseconds
}

// ActionType represents the kind of sync action to perform.
type ActionType int

// Action types produced by the reconciler (sync-algorithm.md section 5.5).
const (
	ActionDownload     ActionType = iota // Pull remote file to local
	ActionUpload                         // Push local file to remote
	ActionLocalDelete                    // Delete local file/folder
	ActionRemoteDelete                   // Delete remote file/folder
	ActionLocalMove                      // Rename/move local file/folder
	ActionRemoteMove                     // Rename/move remote file/folder
	ActionFolderCreate                   // Create folder (local or remote)
	ActionConflict                       // Record and resolve conflict
	ActionUpdateSynced                   // Update synced base (false conflict)
	ActionCleanup                        // Remove stale DB record
)

// FolderCreateSide indicates whether a folder should be created locally or remotely.
type FolderCreateSide int

const (
	FolderCreateLocal  FolderCreateSide = iota + 1 // Create folder on local filesystem
	FolderCreateRemote                             // Create folder via Graph API
)

// SyncMode controls which sides of the sync are active.
type SyncMode int

// Sync direction modes (sync-algorithm.md section 1.5).
const (
	SyncBidirectional SyncMode = iota
	SyncDownloadOnly
	SyncUploadOnly
)

// Action represents a single planned operation produced by the reconciler.
type Action struct {
	Type         ActionType
	DriveID      string
	ItemID       string
	Path         string           // current path
	NewPath      string           // destination path for moves
	CreateSide   FolderCreateSide // only set for ActionFolderCreate
	Item         *Item            // full item state for context
	ConflictInfo *ConflictRecord
}

// ActionPlan is the ordered collection of actions produced by the reconciler,
// grouped by type for correct execution ordering (sync-algorithm.md section 5.3).
type ActionPlan struct {
	FolderCreates []Action // ordered top-down by depth
	Moves         []Action // folder moves first, then file moves
	Downloads     []Action // parallel execution
	Uploads       []Action // parallel execution
	LocalDeletes  []Action // files first, then folders bottom-up
	RemoteDeletes []Action // files first, then folders bottom-up
	Conflicts     []Action // recorded and resolved per policy
	SyncedUpdates []Action // false conflicts and bookkeeping
	Cleanups      []Action // DB record cleanup
}

// TotalActions returns the total number of actions across all categories.
func (p *ActionPlan) TotalActions() int {
	return len(p.FolderCreates) + len(p.Moves) +
		len(p.Downloads) + len(p.Uploads) +
		len(p.LocalDeletes) + len(p.RemoteDeletes) +
		len(p.Conflicts) + len(p.SyncedUpdates) + len(p.Cleanups)
}

// TotalDeletes returns the count of local and remote delete actions.
func (p *ActionPlan) TotalDeletes() int {
	return len(p.LocalDeletes) + len(p.RemoteDeletes)
}

// FilterResult indicates whether an item should be synced and why.
type FilterResult struct {
	Included bool
	Reason   string // empty when included, explanation when excluded
}

// --- Consumer-defined interfaces for graph client ---
// These decouple the sync package from graph's concrete types,
// following the "accept interfaces, return structs" Go convention.

// DeltaFetcher retrieves remote changes from the Graph API.
type DeltaFetcher interface {
	// Delta returns one page of delta results. Pass an empty token for initial sync.
	Delta(ctx context.Context, driveID, token string) (*graph.DeltaPage, error)
}

// ItemClient performs CRUD operations on drive items via the Graph API.
type ItemClient interface {
	GetItem(ctx context.Context, driveID, itemID string) (*graph.Item, error)
	ListChildren(ctx context.Context, driveID, itemID string) ([]graph.Item, error)
	CreateFolder(ctx context.Context, driveID, parentID, name string) (*graph.Item, error)
	MoveItem(ctx context.Context, driveID, itemID, newParentID, newName string) (*graph.Item, error)
	DeleteItem(ctx context.Context, driveID, itemID string) error
}

// TransferClient handles file downloads and uploads via the Graph API.
type TransferClient interface {
	Download(ctx context.Context, driveID, itemID string, w io.Writer) (int64, error)
	SimpleUpload(ctx context.Context, driveID, parentID, name string, r io.Reader, size int64) (*graph.Item, error)
	CreateUploadSession(ctx context.Context, driveID, parentID, name string, mtime time.Time) (*graph.UploadSession, error)
	UploadChunk(ctx context.Context, uploadURL string, r io.Reader, offset, length, totalSize int64) (*graph.Item, error)
}

// Store is the interface for the sync state database. All sync components
// operate against this interface rather than the concrete SQLite implementation.
type Store interface {
	// Items
	GetItem(ctx context.Context, driveID, itemID string) (*Item, error)
	UpsertItem(ctx context.Context, item *Item) error
	MarkDeleted(ctx context.Context, driveID, itemID string, deletedAt int64) error
	ListChildren(ctx context.Context, driveID, parentID string) ([]*Item, error)
	GetItemByPath(ctx context.Context, path string) (*Item, error)
	ListAllActiveItems(ctx context.Context) ([]*Item, error)
	ListSyncedItems(ctx context.Context) ([]*Item, error)
	BatchUpsert(ctx context.Context, items []*Item) error

	// Path materialization
	MaterializePath(ctx context.Context, driveID, itemID string) (string, error)
	CascadePathUpdate(ctx context.Context, oldPrefix, newPrefix string) error

	// Tombstone lifecycle
	CleanupTombstones(ctx context.Context, retentionDays int) (int64, error)

	// Delta tokens
	GetDeltaToken(ctx context.Context, driveID string) (string, error)
	SaveDeltaToken(ctx context.Context, driveID, token string) error
	DeleteDeltaToken(ctx context.Context, driveID string) error
	SetDeltaComplete(ctx context.Context, driveID string, complete bool) error
	IsDeltaComplete(ctx context.Context, driveID string) (bool, error)

	// Conflicts
	RecordConflict(ctx context.Context, record *ConflictRecord) error
	ListConflicts(ctx context.Context, driveID string) ([]*ConflictRecord, error)
	ResolveConflict(ctx context.Context, id string, resolution ConflictResolution, resolvedBy ConflictResolvedBy) error
	ConflictCount(ctx context.Context, driveID string) (int, error)

	// Stale files
	RecordStaleFile(ctx context.Context, record *StaleRecord) error
	ListStaleFiles(ctx context.Context) ([]*StaleRecord, error)
	RemoveStaleFile(ctx context.Context, id string) error

	// Upload sessions
	SaveUploadSession(ctx context.Context, record *UploadSessionRecord) error
	GetUploadSession(ctx context.Context, id string) (*UploadSessionRecord, error)
	DeleteUploadSession(ctx context.Context, id string) error
	ListExpiredSessions(ctx context.Context, now int64) ([]*UploadSessionRecord, error)

	// Config snapshot (for stale file detection on filter changes)
	GetConfigSnapshot(ctx context.Context, key string) (string, error)
	SaveConfigSnapshot(ctx context.Context, key, value string) error

	// Maintenance
	Checkpoint() error
	Close() error
}

// Filter determines whether a file or directory should be included in sync.
// It encapsulates the three-layer filter cascade (sync-algorithm.md section 6).
type Filter interface {
	ShouldSync(path string, isDir bool, size int64) FilterResult
}

// --- Timestamp helpers ---
// All internal code uses int64 Unix nanoseconds exclusively.
// Conversion happens at system boundaries only (data-model.md section 13).

// NowNano returns the current time as Unix nanoseconds.
func NowNano() int64 {
	return time.Now().UnixNano()
}

// ToUnixNano converts a time.Time to Unix nanoseconds.
// Returns 0 for the zero time.
func ToUnixNano(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}

	return t.UnixNano()
}

// secondsPerNano is the divisor to truncate nanoseconds to seconds precision.
const secondsPerNano = int64(time.Second)

// TruncateToSeconds truncates a nanosecond timestamp to whole-second precision.
// OneDrive does not store fractional seconds, so comparison must use truncated values
// to avoid false positives from filesystem timestamp precision differences.
func TruncateToSeconds(ns int64) int64 {
	return (ns / secondsPerNano) * secondsPerNano
}

// Int64Ptr returns a pointer to the given int64 value.
// Used for nullable database columns.
func Int64Ptr(v int64) *int64 {
	return &v
}

// NewFilterConfig extracts the filter configuration needed by the filter engine
// from a resolved drive configuration.
func NewFilterConfig(resolved *config.ResolvedDrive) config.FilterConfig {
	return resolved.FilterConfig
}

// NewSafetyConfig extracts the safety configuration needed by the safety checker
// from a resolved drive configuration. Returns a pointer because SafetyConfig
// is 88 bytes — exceeds gocritic's hugeParam threshold.
func NewSafetyConfig(resolved *config.ResolvedDrive) *config.SafetyConfig {
	cfg := resolved.SafetyConfig
	return &cfg
}
