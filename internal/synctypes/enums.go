// Package synctypes defines shared vocabulary types used across the sync
// subsystem packages. It is a leaf package with no internal dependencies
// beyond standard library and driveid.
package synctypes

import (
	"database/sql/driver"
	"fmt"
)

// String constants for enum serialization (shared by String() and Parse*).
const (
	StrRemote       = "remote"
	StrLocal        = "local"
	StrFile         = "file"
	StrFolder       = "folder"
	StrRoot         = "root"
	StrDownload     = "download"
	StrUpload       = "upload"
	StrDelete       = "delete"
	StrActionable   = "actionable"
	StrTransient    = "transient"
	StrLocalDelete  = "local_delete"
	StrRemoteDelete = "remote_delete"
	StrLocalMove    = "local_move"
	StrRemoteMove   = "remote_move"
	StrFolderCreate = "folder_create"
	StrConflict     = "conflict"
	StrUpdateSynced = "update_synced"
	StrCleanup      = "cleanup"

	// SyncStatus string constants — remote_state sync_status column values.
	StrPendingDownload = "pending_download"
	StrDownloading     = "downloading"
	StrDownloadFailed  = "download_failed"
	StrSynced          = "synced"
	StrPendingDelete   = "pending_delete"
	StrDeleting        = "deleting"
	StrDeleteFailed    = "delete_failed"
	StrDeleted         = "deleted"
	StrFiltered        = "filtered"
)

// Direction represents the direction of a sync action (upload, download, delete).
// Stored as TEXT in SQLite — type Direction string serializes identically to
// raw strings, so no migration is needed.
type Direction string

const (
	DirectionDownload Direction = StrDownload
	DirectionUpload   Direction = StrUpload
	DirectionDelete   Direction = StrDelete
)

// FailureCategory classifies sync failures as transient (retryable) or
// actionable (requires user intervention).
type FailureCategory string

const (
	CategoryTransient  FailureCategory = StrTransient
	CategoryActionable FailureCategory = StrActionable
)

// SyncStatus represents the sync_status of a remote_state row. Stored as TEXT
// in SQLite — type SyncStatus string serializes identically to raw strings,
// so no migration is needed. Matches the CHECK constraint in the remote_state
// table (migrations/00001_consolidated_schema.sql).
type SyncStatus string

const (
	SyncStatusPendingDownload SyncStatus = StrPendingDownload
	SyncStatusDownloading     SyncStatus = StrDownloading
	SyncStatusDownloadFailed  SyncStatus = StrDownloadFailed
	SyncStatusSynced          SyncStatus = StrSynced
	SyncStatusPendingDelete   SyncStatus = StrPendingDelete
	SyncStatusDeleting        SyncStatus = StrDeleting
	SyncStatusDeleteFailed    SyncStatus = StrDeleteFailed
	SyncStatusDeleted         SyncStatus = StrDeleted
	SyncStatusFiltered        SyncStatus = StrFiltered
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
		return StrRemote
	case SourceLocal:
		return StrLocal
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
		return StrFile
	case ItemTypeFolder:
		return StrFolder
	case ItemTypeRoot:
		return StrRoot
	default:
		return fmt.Sprintf("ItemType(%d)", int(t))
	}
}

// Scan implements sql.Scanner so database/sql can scan a TEXT column
// directly into an ItemType field. This eliminates manual ParseItemType
// calls at every consumption point.
func (t *ItemType) Scan(src any) error {
	s, ok := src.(string)
	if !ok {
		return fmt.Errorf("synctypes: ItemType.Scan: expected string, got %T", src)
	}

	parsed, err := ParseItemType(s)
	if err != nil {
		return err
	}

	*t = parsed

	return nil
}

// Value implements driver.Valuer so database/sql can bind an ItemType
// field as a TEXT parameter in SQL statements.
func (t ItemType) Value() (driver.Value, error) {
	return t.String(), nil
}

// ParseItemType converts a database TEXT value to ItemType.
func ParseItemType(s string) (ItemType, error) {
	switch s {
	case StrFile:
		return ItemTypeFile, nil
	case StrFolder:
		return ItemTypeFolder, nil
	case StrRoot:
		return ItemTypeRoot, nil
	default:
		return ItemTypeFile, fmt.Errorf("synctypes: unknown item type %q", s)
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
		return StrDownload
	case ActionUpload:
		return StrUpload
	case ActionLocalDelete:
		return StrLocalDelete
	case ActionRemoteDelete:
		return StrRemoteDelete
	case ActionLocalMove:
		return StrLocalMove
	case ActionRemoteMove:
		return StrRemoteMove
	case ActionFolderCreate:
		return StrFolderCreate
	case ActionConflict:
		return StrConflict
	case ActionUpdateSynced:
		return StrUpdateSynced
	case ActionCleanup:
		return StrCleanup
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
		return StrLocal
	case CreateRemote:
		return StrRemote
	default:
		return fmt.Sprintf("FolderCreateSide(%d)", int(s))
	}
}
