package sync

import "github.com/tonimelisma/onedrive-go/internal/driveid"

// DeferredCounts summarizes action classes observed by the planner but
// suppressed by the selected sync direction. Permission-driven suppression is
// intentionally excluded; this structure is only for user-visible directional
// deferral reporting.
type DeferredCounts struct {
	FolderCreates int
	Moves         int
	Downloads     int
	Uploads       int
	LocalDeletes  int
	RemoteDeletes int
}

// Total returns the number of deferred actions across the supported
// direction-suppressible categories.
func (c DeferredCounts) Total() int {
	return c.FolderCreates + c.Moves + c.Downloads + c.Uploads +
		c.LocalDeletes + c.RemoteDeletes
}

// AddAction increments the matching deferred bucket for an action type that
// can be suppressed by sync direction. Conflict and bookkeeping actions are
// intentionally ignored.
func (c *DeferredCounts) AddAction(action *Action) {
	if c == nil || action == nil {
		return
	}

	switch action.Type {
	case ActionFolderCreate:
		c.FolderCreates++
	case ActionLocalMove, ActionRemoteMove:
		c.Moves++
	case ActionDownload:
		c.Downloads++
	case ActionUpload:
		c.Uploads++
	case ActionLocalDelete:
		c.LocalDeletes++
	case ActionRemoteDelete:
		c.RemoteDeletes++
	case ActionConflictCopy, ActionUpdateSynced, ActionCleanup:
		return
	}
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

// ThrottleTargetKey returns the narrowest remote boundary that can be blocked
// after a 429 for this action. The engine currently scopes throttle blocks by
// action drive, regardless of whether the sync drive is rooted at drive root
// or below a mount root.
func (a *Action) ThrottleTargetKey() string {
	if a == nil {
		return ""
	}
	if a.DriveID.IsZero() {
		return ""
	}
	return throttleDriveParam(a.DriveID)
}

// ActionPlan contains a flat list of actions with explicit dependency edges.
// The Deps adjacency list encodes ordering constraints (parent-before-child,
// children-before-parent-delete, move-target-parent).
type ActionPlan struct {
	Actions        []Action       // flat list of all executable actions
	Deps           [][]int        // Deps[i] = indices that action i depends on
	DeferredByMode DeferredCounts // planner-observed work suppressed by direction
}

// ActionOutcome is the result of executing a single action. Self-contained —
// has everything the SyncStore needs to update the database.
type ActionOutcome struct {
	Action            ActionType
	Success           bool
	Error             error
	Path              string
	FailurePath       string
	OldPath           string // for moves
	DriveID           driveid.ID
	ItemID            string // from API response after upload
	ParentID          string
	ItemType          ItemType
	FailureCapability PermissionCapability
	LocalHash         string
	RemoteHash        string
	LocalSize         int64
	LocalSizeKnown    bool
	RemoteSize        int64
	RemoteSizeKnown   bool
	LocalMtime        int64 // local mtime at sync time
	RemoteMtime       int64 // remote mtime at sync time; zero means unknown
	ETag              string
	ConflictType      string // ConflictEditDelete etc. (conflicts only)
	ResolvedBy        string // ResolvedByAuto for auto-resolved conflicts, "" otherwise
}
