package sync

import (
	"context"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// SkippedItem records a local filesystem entry that was rejected at
// observation time.
type SkippedItem struct {
	Path     string
	Reason   string
	Detail   string
	FileSize int64
}

// ScanResult is the return type of FullScan.
type ScanResult struct {
	Events  []ChangeEvent
	Skipped []SkippedItem
}

// ChangeEvent is an immutable observation of a change, produced by observers
// and consumed by the change buffer and planner.
type ChangeEvent struct {
	Source          synctypes.ChangeSource
	Type            synctypes.ChangeType
	ForcedAction    synctypes.ActionType
	HasForcedAction bool
	Path            string
	OldPath         string
	ItemID          string
	ParentID        string
	DriveID         driveid.ID
	ItemType        synctypes.ItemType
	Name            string
	Size            int64
	Hash            string
	Mtime           int64
	ETag            string
	CTag            string
	IsDeleted       bool
	RemoteDriveID   string
	RemoteItemID    string
}

// PathChanges groups all change events for a single path, separating
// remote and local observations.
type PathChanges struct {
	Path         string
	RemoteEvents []ChangeEvent
	LocalEvents  []ChangeEvent
}

// RemoteState captures the current state of a path as observed from the Graph API.
type RemoteState struct {
	ItemID    string
	DriveID   driveid.ID
	ParentID  string
	Name      string
	ItemType  synctypes.ItemType
	Size      int64
	Hash      string
	Mtime     int64
	ETag      string
	CTag      string
	IsDeleted bool

	RemoteDriveID string
	RemoteItemID  string
}

// LocalState captures the current state of a path as observed from the local filesystem.
type LocalState struct {
	Name     string
	ItemType synctypes.ItemType
	Size     int64
	Hash     string
	Mtime    int64
}

// PathView is a unified three-way view of a single path.
type PathView struct {
	Path            string
	Remote          *RemoteState
	Local           *LocalState
	Baseline        *syncstore.BaselineEntry
	ForcedAction    synctypes.ActionType
	HasForcedAction bool
}

// Shortcut represents a OneDrive shortcut or shared folder that requires
// separate observation on the source drive.
type Shortcut struct {
	ItemID       string
	RemoteDrive  string
	RemoteItem   string
	LocalPath    string
	DriveType    string
	Observation  string
	DiscoveredAt int64
}

const (
	ObservationUnknown   = "unknown"
	ObservationDelta     = "delta"
	ObservationEnumerate = "enumerate"

	throttleSharedPrefix = "shared:"
)

func throttleDriveParam(targetDriveID driveid.ID) string {
	return "drive:" + targetDriveID.String()
}

// Action is an instruction for the executor, produced by the planner.
type Action struct {
	Type                synctypes.ActionType
	DriveID             driveid.ID
	ItemID              string
	Path                string
	OldPath             string
	CreateSide          synctypes.FolderCreateSide
	View                *PathView
	ConflictInfo        *syncstore.ConflictRecord
	TargetShortcutKey   string
	TargetDriveID       driveid.ID
	TargetRootItemID    string
	TargetRootLocalPath string
}

func (a *Action) TargetsOwnDrive() bool {
	return a.TargetShortcutKey == ""
}

func (a *Action) ShortcutKey() string {
	return a.TargetShortcutKey
}

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
type ActionPlan struct {
	Actions []Action
	Deps    [][]int
}

// ExecutionResult is the result of executing a single action.
type ExecutionResult struct {
	Action          synctypes.ActionType
	Success         bool
	Error           error
	Path            string
	OldPath         string
	DriveID         driveid.ID
	ItemID          string
	ParentID        string
	ItemType        synctypes.ItemType
	LocalHash       string
	RemoteHash      string
	LocalSize       int64
	LocalSizeKnown  bool
	RemoteSize      int64
	RemoteSizeKnown bool
	LocalMtime      int64
	RemoteMtime     int64
	ETag            string
	ConflictType    string
	ResolvedBy      string
}

func baselineMutationFromExecutionResult(result *ExecutionResult) *syncstore.BaselineMutation {
	return &syncstore.BaselineMutation{
		Action:          result.Action,
		Success:         result.Success,
		Path:            result.Path,
		OldPath:         result.OldPath,
		DriveID:         result.DriveID,
		ItemID:          result.ItemID,
		ParentID:        result.ParentID,
		ItemType:        result.ItemType,
		LocalHash:       result.LocalHash,
		RemoteHash:      result.RemoteHash,
		LocalSize:       result.LocalSize,
		LocalSizeKnown:  result.LocalSizeKnown,
		RemoteSize:      result.RemoteSize,
		RemoteSizeKnown: result.RemoteSizeKnown,
		LocalMtime:      result.LocalMtime,
		RemoteMtime:     result.RemoteMtime,
		ETag:            result.ETag,
		ConflictType:    result.ConflictType,
		ResolvedBy:      result.ResolvedBy,
	}
}

// TrackedAction pairs an Action with an ID and a per-action cancel function.
type TrackedAction struct {
	Action        Action
	ID            int64
	Cancel        context.CancelFunc
	IsTrial       bool
	TrialScopeKey synctypes.ScopeKey
}

// WorkerResult reports the outcome of a single action execution.
type WorkerResult struct {
	Path          string
	ItemID        string
	DriveID       driveid.ID
	ActionType    synctypes.ActionType
	Success       bool
	ErrMsg        string
	HTTPStatus    int
	Err           error
	RetryAfter    time.Duration
	TargetDriveID driveid.ID
	ShortcutKey   string
	IsTrial       bool
	TrialScopeKey synctypes.ScopeKey
	ActionID      int64
}

func (r *WorkerResult) ThrottleTargetKey() string {
	if r == nil {
		return ""
	}
	if r.ShortcutKey != "" {
		return throttleSharedPrefix + r.ShortcutKey
	}
	targetDriveID := r.TargetDriveID
	if targetDriveID.IsZero() {
		targetDriveID = r.DriveID
	}
	if targetDriveID.IsZero() {
		return ""
	}
	return throttleDriveParam(targetDriveID)
}
