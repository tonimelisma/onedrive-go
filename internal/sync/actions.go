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
// can be suppressed by sync direction. Conflict-copy and publication-only
// actions are intentionally ignored.
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
	case ActionConflictCopy, ActionBaselineUpdate, ActionCleanup:
		return
	}
}

// Action is an instruction for the executor, produced by the planner.
type Action struct {
	Type                      actionType
	DriveID                   driveid.ID
	ItemID                    string
	Path                      string           // canonical path (destination for moves)
	OldPath                   string           // source path (moves only)
	CreateSide                folderCreateSide // for folder creates
	View                      *pathView        // full three-way context
	RequireMissingLocalTarget bool             // downloads after preserving a local conflict copy
	// MovedLocal is the local truth the planner observed at Path for a
	// local-origin move (ActionRemoteMove). View describes the source at
	// OldPath, which is absent locally, so this is the only record of what
	// now sits at the destination. Freshness checks the destination against
	// it, and the move outcome publishes it as the baseline's local half.
	MovedLocal *localState
	// MovedLocalPath is where MovedLocal sits in the local snapshot when that
	// is not Path: a move composed under a parent that an earlier move of the
	// same plan puts in place (planner_move_composition.go). local_state is
	// observation-owned and does not follow the plan's own moves, so
	// freshness reads the moved item there.
	MovedLocalPath string
	// MovedRemotePath is where remote_state holds the item of a remote-origin
	// move (ActionLocalMove) when that is not Path: a child renamed inside a
	// folder before an OneDrive rename of the folder is applied locally.
	MovedRemotePath string
	// ReplaceLocalTarget lets a remote-origin local move replace the entry
	// planned at its destination (View.Local): a synced file OneDrive no
	// longer has, which the move verifies against that plan, keeps in the
	// trash, and then replaces (D-058).
	ReplaceLocalTarget bool
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

// actionPlan contains a flat list of actions with explicit dependency edges.
// The Deps adjacency list encodes ordering constraints (parent-before-child,
// children-before-parent-delete, move-target-parent).
type actionPlan struct {
	Actions        []Action       // flat list of all executable actions
	Deps           [][]int        // Deps[i] = indices that action i depends on
	DeferredByMode DeferredCounts // planner-observed work suppressed by direction
	// HeldDeletes are deletes the delete-safety hold kept out of Actions
	// until the user confirms them (planner_delete_safety.go).
	HeldDeletes []heldDelete
}

// actionOutcome is the result of executing a single action. Self-contained —
// has everything the SyncStore needs to update the database.
type actionOutcome struct {
	Action                actionType
	Success               bool
	Error                 error
	Path                  string
	FailurePath           string
	OldPath               string // for moves
	DriveID               driveid.ID
	ItemID                string // from API response after upload
	ParentID              string
	ItemType              ItemType
	FailureCapability     permissionCapability
	LocalHash             string
	RemoteHash            string
	LocalSize             int64
	LocalSizeKnown        bool
	LocalCtime            int64 // local inode change time; recorded with identity
	LocalDevice           uint64
	LocalInode            uint64
	LocalHasIdentity      bool
	LocalIdentityObserved bool
	RemoteSize            int64
	RemoteSizeKnown       bool
	LocalMtime            int64 // local mtime at sync time
	RemoteMtime           int64 // remote mtime at sync time; zero means unknown
	ETag                  string
}
