package synctypes

import "github.com/tonimelisma/onedrive-go/internal/driveid"

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

	// TargetShortcutKey identifies the shortcut scope for scope-based failure
	// handling. Format: "remoteDrive:remoteItem". Empty for own-drive actions.
	// Populated by the planner for shortcut-targeted actions (R-6.8.13).
	TargetShortcutKey string

	// TargetDriveID is the actual drive ID targeted by this action. For own-drive
	// actions, equals DriveID. For shortcut actions, equals the remote drive.
	// Flows through the pipeline without lookup (R-6.8.12).
	TargetDriveID driveid.ID
}

// TargetsOwnDrive returns true if this action targets the user's own drive.
// Used by scope detection to determine scope key for 507/quota failures (R-6.8.13).
func (a *Action) TargetsOwnDrive() bool {
	return a.TargetShortcutKey == ""
}

// ShortcutKey returns "remoteDrive:remoteItem" for shortcut-targeted actions,
// empty for own-drive actions. Used as the scope key suffix for shortcut-scoped
// quota failures (R-6.8.13).
func (a *Action) ShortcutKey() string {
	return a.TargetShortcutKey
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
	Action          ActionType
	Success         bool
	Error           error
	Path            string
	OldPath         string // for moves
	DriveID         driveid.ID
	ItemID          string // from API response after upload
	ParentID        string
	ItemType        ItemType
	LocalHash       string
	RemoteHash      string
	LocalSize       int64
	LocalSizeKnown  bool
	RemoteSize      int64
	RemoteSizeKnown bool
	LocalMtime      int64 // local mtime at sync time
	RemoteMtime     int64 // remote mtime at sync time; zero means unknown
	ETag            string
	ConflictType    string // ConflictEditDelete etc. (conflicts only)
	ResolvedBy      string // ResolvedByAuto for auto-resolved conflicts, "" otherwise
}
