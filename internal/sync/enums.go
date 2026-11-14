// Sync-domain enums and shared value vocabulary.

package sync

import (
	"database/sql/driver"
	"fmt"
)

// outsideTheEnum describes a value none of an enum's constants has: the panic
// value of a decision that met one, and the text of the error a decision that
// returns errors refuses one with. Neither enum it is used for can carry one
// to a decision: the planner refuses an action whose type or folder create
// side is outside its enum before any rule reads it (validatePlannedActions),
// and the store refuses an action type outside the enum on read and on write
// (parseActionType, Value). Reaching it is a programming error, so the
// decision stops rather than answer with a verdict no case states.
// TestActionTypeSwitches_EveryDefaultRefuses holds every switch on the two
// enums to it.
func outsideTheEnum(value fmt.Stringer) string {
	return value.String() + " is outside its enum"
}

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

// isMember reports whether a is one of the constants above. The exhaustive
// linter holds this switch, like every other switch on the action type, to
// every member, so lint passes only once a new member is decided everywhere.
func (a actionType) isMember() bool {
	switch a {
	case ActionDownload, ActionUpload, ActionLocalDelete, ActionRemoteDelete,
		ActionLocalMove, ActionRemoteMove, ActionFolderCreate, ActionConflictCopy,
		ActionBaselineUpdate, ActionCleanup:
		return true
	default:
		return false
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

// Value implements driver.Valuer for ActionType TEXT columns. A value outside
// the enum is refused, so no store row can carry one.
func (a actionType) Value() (driver.Value, error) {
	if !a.isMember() {
		return nil, fmt.Errorf("sync: invalid ActionType %d", a)
	}

	return a.String(), nil
}

// folderCreateSide specifies where a new folder should be created.
type folderCreateSide int

const (
	createLocal folderCreateSide = iota
	CreateRemote
)

// isMember reports whether s is one of the constants above.
func (s folderCreateSide) isMember() bool {
	switch s {
	case createLocal, CreateRemote:
		return true
	default:
		return false
	}
}

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
