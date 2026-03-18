// Package synctypes defines shared vocabulary types used across the sync
// subsystem packages. It is a leaf package with no internal dependencies
// beyond standard library and driveid.
package synctypes

import (
	"database/sql/driver"
	"fmt"
)

// string constants for enum serialization (shared by String() and Parse*).
// Unexported: only used within this file for typed constant definitions
// and String()/Parse* methods.
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

	// SyncStatus string constants — remote_state sync_status column values.
	strPendingDownload = "pending_download"
	strDownloading     = "downloading"
	strDownloadFailed  = "download_failed"
	strSynced          = "synced"
	strPendingDelete   = "pending_delete"
	strDeleting        = "deleting"
	strDeleteFailed    = "delete_failed"
	strDeleted         = "deleted"
	strFiltered        = "filtered"
)

// Direction represents the direction of a sync action (upload, download, delete).
// Stored as TEXT in SQLite — type Direction string serializes identically to
// raw strings, so no migration is needed.
type Direction string

const (
	DirectionDownload Direction = strDownload
	DirectionUpload   Direction = strUpload
	DirectionDelete   Direction = strDelete
)

// FailureCategory classifies sync failures as transient (retryable) or
// actionable (requires user intervention).
type FailureCategory string

const (
	CategoryTransient  FailureCategory = strTransient
	CategoryActionable FailureCategory = strActionable
)

// SyncStatus represents the sync_status of a remote_state row. Stored as TEXT
// in SQLite — type SyncStatus string serializes identically to raw strings,
// so no migration is needed. Matches the CHECK constraint in the remote_state
// table (migrations/00001_consolidated_schema.sql).
type SyncStatus string

const (
	SyncStatusPendingDownload SyncStatus = strPendingDownload
	SyncStatusDownloading     SyncStatus = strDownloading
	SyncStatusDownloadFailed  SyncStatus = strDownloadFailed
	SyncStatusSynced          SyncStatus = strSynced
	SyncStatusPendingDelete   SyncStatus = strPendingDelete
	SyncStatusDeleting        SyncStatus = strDeleting
	SyncStatusDeleteFailed    SyncStatus = strDeleteFailed
	SyncStatusDeleted         SyncStatus = strDeleted
	SyncStatusFiltered        SyncStatus = strFiltered
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
	case strFile:
		return ItemTypeFile, nil
	case strFolder:
		return ItemTypeFolder, nil
	case strRoot:
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
