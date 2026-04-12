package sync

import (
	"github.com/tonimelisma/onedrive-go/internal/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

type (
	SkippedItem     = synctypes.SkippedItem
	ScanResult      = synctypes.ScanResult
	ChangeEvent     = synctypes.ChangeEvent
	PathChanges     = synctypes.PathChanges
	RemoteState     = synctypes.RemoteState
	LocalState      = synctypes.LocalState
	PathView        = synctypes.PathView
	Shortcut        = synctypes.Shortcut
	Action          = synctypes.Action
	ActionPlan      = synctypes.ActionPlan
	ExecutionResult = synctypes.Outcome
)

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

type TrackedAction = synctypes.TrackedAction

type WorkerResult = synctypes.WorkerResult
