// Package sync defines sync-domain enums and shared value vocabulary.
package sync

import (
	"database/sql/driver"
	"fmt"
)

// string constants for enum serialization (shared by String() and Parse*).
// Unexported: only used within this file for typed constant definitions
// and String()/Parse* methods.
const (
	strRemote         = "remote"
	strLocal          = "local"
	strCreate         = "create"
	strModify         = "modify"
	strFile           = "file"
	strFolder         = "folder"
	strRoot           = "root"
	strDownload       = "download"
	strUpload         = "upload"
	strDelete         = "delete"
	strLocalDelete    = "local_delete"
	strRemoteDelete   = "remote_delete"
	strLocalMove      = "local_move"
	strRemoteMove     = "remote_move"
	strMove           = "move"
	strFolderCreate   = "folder_create"
	strConflictCopy   = "conflict_copy"
	strBaselineUpdate = "baseline_update"
	strCleanup        = "cleanup"
)

// direction represents the direction of a sync action (upload, download, delete).
// Stored as TEXT in SQLite — type direction string serializes identically to
// raw strings, so no compatibility rewrite is needed.
type direction string

const (
	directionDownload direction = strDownload
	directionUpload   direction = strUpload
	directionDelete   direction = strDelete
)

// changeSource identifies the origin of a change event.
type changeSource int

const (
	// sourceRemote indicates the change was observed from the Graph API.
	sourceRemote changeSource = iota
	// SourceLocal indicates the change was observed from the local filesystem.
	SourceLocal
)

func (s changeSource) String() string {
	switch s {
	case sourceRemote:
		return strRemote
	case SourceLocal:
		return strLocal
	default:
		return fmt.Sprintf("ChangeSource(%d)", int(s))
	}
}

// changeType classifies what kind of change occurred.
type changeType int

const (
	changeCreate changeType = iota
	ChangeModify
	ChangeDelete
	ChangeMove
)

func (t changeType) String() string {
	switch t {
	case changeCreate:
		return strCreate
	case ChangeModify:
		return strModify
	case ChangeDelete:
		return strDelete
	case ChangeMove:
		return strMove
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
		return fmt.Errorf("sync: ItemType.Scan: expected string, got %T", src)
	}

	parsed, err := parseItemType(s)
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

// parseItemType converts a database TEXT value to ItemType.
func parseItemType(s string) (ItemType, error) {
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

// actionType classifies what the executor should do for a given action.
type actionType int

const (
	ActionDownload actionType = iota
	ActionUpload
	ActionLocalDelete
	ActionRemoteDelete
	ActionLocalMove
	ActionRemoteMove
	ActionFolderCreate
	ActionConflictCopy
	ActionBaselineUpdate
	ActionCleanup
)

func (a actionType) String() string {
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
	case ActionConflictCopy:
		return strConflictCopy
	case ActionBaselineUpdate:
		return strBaselineUpdate
	case ActionCleanup:
		return strCleanup
	default:
		return fmt.Sprintf("ActionType(%d)", int(a))
	}
}

// Direction returns the coarse sync direction that owns this action type for
// persistence and display. Retry/trial rebuild logic must branch on ActionType
// directly, but retry_work still keeps Direction for coarse summaries and
// query filtering.
func (a actionType) Direction() direction {
	switch a {
	case ActionUpload:
		return directionUpload
	case ActionLocalDelete, ActionRemoteDelete:
		return directionDelete
	case ActionDownload, ActionFolderCreate, ActionConflictCopy,
		ActionLocalMove, ActionRemoteMove, ActionBaselineUpdate, ActionCleanup:
		return directionDownload
	default:
		return directionDownload
	}
}

// parseActionType converts the SQLite wire value back into an ActionType.
func parseActionType(s string) (actionType, error) {
	switch s {
	case strDownload:
		return ActionDownload, nil
	case strUpload:
		return ActionUpload, nil
	case strLocalDelete:
		return ActionLocalDelete, nil
	case strRemoteDelete:
		return ActionRemoteDelete, nil
	case strLocalMove:
		return ActionLocalMove, nil
	case strRemoteMove:
		return ActionRemoteMove, nil
	case strFolderCreate:
		return ActionFolderCreate, nil
	case strConflictCopy:
		return ActionConflictCopy, nil
	case strBaselineUpdate:
		return ActionBaselineUpdate, nil
	case strCleanup:
		return ActionCleanup, nil
	default:
		return 0, fmt.Errorf("sync: invalid ActionType %q", s)
	}
}

// Scan implements sql.Scanner for ActionType TEXT columns.
func (a *actionType) Scan(src any) error {
	s, ok := src.(string)
	if !ok {
		return fmt.Errorf("sync: ActionType.Scan: expected string, got %T", src)
	}

	parsed, err := parseActionType(s)
	if err != nil {
		return err
	}

	*a = parsed

	return nil
}

// Value implements driver.Valuer for ActionType TEXT columns.
func (a actionType) Value() (driver.Value, error) {
	switch a {
	case ActionDownload, ActionUpload, ActionLocalDelete, ActionRemoteDelete,
		ActionLocalMove, ActionRemoteMove, ActionFolderCreate, ActionConflictCopy,
		ActionBaselineUpdate, ActionCleanup:
		return a.String(), nil
	default:
		return nil, fmt.Errorf("sync: invalid ActionType %d", a)
	}
}

// folderCreateSide specifies where a new folder should be created.
type folderCreateSide int

const (
	createLocal folderCreateSide = iota
	CreateRemote
)

func (s folderCreateSide) String() string {
	switch s {
	case createLocal:
		return strLocal
	case CreateRemote:
		return strRemote
	default:
		return fmt.Sprintf("FolderCreateSide(%d)", int(s))
	}
}
