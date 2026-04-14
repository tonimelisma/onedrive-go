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
	case ActionConflict, ActionUpdateSynced, ActionCleanup:
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

	// TargetShortcutKey identifies the shortcut scope for scope-based failure
	// handling. Format: "remoteDrive:remoteItem". Empty for own-drive actions.
	// Populated by the planner for shortcut-targeted actions (R-6.8.13).
	TargetShortcutKey string

	// TargetDriveID is the actual drive ID targeted by this action. For own-drive
	// actions, equals DriveID. For shortcut actions, equals the remote drive.
	// Flows through the pipeline without lookup (R-6.8.12).
	TargetDriveID driveid.ID

	// TargetRootItemID identifies the shortcut root item that owns this action's
	// remote path when the action targets a foreign drive subtree.
	TargetRootItemID string

	// TargetRootLocalPath is the local sync path corresponding to TargetRootItemID.
	// Cross-drive convergence strips this prefix to compute the target-drive-
	// relative path after a successful mutation.
	TargetRootLocalPath string
}

// PermissionCapabilities returns the concrete capabilities this action
// requires when admitted against active permission scopes.
func (a *Action) PermissionCapabilities() []PermissionCapability {
	if a == nil {
		return nil
	}

	switch a.Type {
	case ActionUpload:
		return uniqueCapabilities(PermissionCapabilityLocalRead, PermissionCapabilityRemoteWrite)
	case ActionDownload:
		return uniqueCapabilities(PermissionCapabilityRemoteRead, PermissionCapabilityLocalWrite)
	case ActionLocalDelete:
		return uniqueCapabilities(PermissionCapabilityLocalWrite)
	case ActionRemoteDelete:
		return uniqueCapabilities(PermissionCapabilityRemoteWrite)
	case ActionLocalMove:
		return uniqueCapabilities(PermissionCapabilityLocalWrite)
	case ActionRemoteMove:
		return uniqueCapabilities(PermissionCapabilityRemoteWrite)
	case ActionFolderCreate:
		if a.CreateSide == CreateLocal {
			return uniqueCapabilities(PermissionCapabilityLocalWrite)
		}
		if a.CreateSide == CreateRemote {
			return uniqueCapabilities(PermissionCapabilityRemoteWrite)
		}
		return nil
	case ActionConflict:
		if a.ConflictInfo != nil && a.ConflictInfo.ConflictType == ConflictEditDelete {
			return uniqueCapabilities(PermissionCapabilityLocalRead, PermissionCapabilityRemoteWrite)
		}
		return uniqueCapabilities(PermissionCapabilityRemoteRead, PermissionCapabilityLocalWrite)
	case ActionUpdateSynced, ActionCleanup:
		return nil
	default:
		return nil
	}
}

// ScopePathsForCapability returns the action paths relevant to the given
// permission capability when matching recursive scope boundaries.
func (a *Action) ScopePathsForCapability(capability PermissionCapability) []string {
	if a == nil {
		return nil
	}

	switch capability {
	case PermissionCapabilityLocalRead:
		return nonEmptyPaths(a.Path)
	case PermissionCapabilityRemoteRead:
		return nonEmptyPaths(a.Path)
	case PermissionCapabilityLocalWrite:
		switch a.Type {
		case ActionLocalMove:
			return nonEmptyPaths(a.Path, a.OldPath)
		case ActionDownload,
			ActionUpload,
			ActionLocalDelete,
			ActionRemoteDelete,
			ActionRemoteMove,
			ActionFolderCreate,
			ActionConflict,
			ActionUpdateSynced,
			ActionCleanup:
			return nonEmptyPaths(a.Path)
		default:
			return nil
		}
	case PermissionCapabilityRemoteWrite:
		switch a.Type {
		case ActionRemoteMove:
			return nonEmptyPaths(a.OldPath, a.Path)
		case ActionDownload,
			ActionUpload,
			ActionLocalDelete,
			ActionRemoteDelete,
			ActionLocalMove,
			ActionFolderCreate,
			ActionConflict,
			ActionUpdateSynced,
			ActionCleanup:
			return nonEmptyPaths(a.Path)
		default:
			return nil
		}
	case PermissionCapabilityUnknown:
		return nil
	default:
		return nil
	}
}

func nonEmptyPaths(paths ...string) []string {
	if len(paths) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(paths))
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		if path == "" {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		out = append(out, path)
	}

	return out
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

// ThrottleTargetKey returns the narrowest remote boundary that can be blocked
// after a 429 for this action. Own-drive actions key by target drive; shared
// actions key by the shared root/item boundary.
func (a *Action) ThrottleTargetKey() string {
	if a == nil {
		return ""
	}

	if a.TargetShortcutKey != "" {
		return throttleSharedPrefix + a.TargetShortcutKey
	}
	targetDriveID := a.TargetDriveID
	if targetDriveID.IsZero() {
		targetDriveID = a.DriveID
	}
	if targetDriveID.IsZero() {
		return ""
	}
	return throttleDriveParam(targetDriveID)
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
	OldPath           string // for moves
	DriveID           driveid.ID
	ItemID            string // from API response after upload
	ParentID          string
	ItemType          ItemType
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
	FailurePath       string
	FailureCapability PermissionCapability
}
