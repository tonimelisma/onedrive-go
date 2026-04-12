package sync

import (
	"github.com/tonimelisma/onedrive-go/internal/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

type ActionType = synctypes.ActionType

const (
	ActionDownload     = synctypes.ActionDownload
	ActionUpload       = synctypes.ActionUpload
	ActionLocalDelete  = synctypes.ActionLocalDelete
	ActionRemoteDelete = synctypes.ActionRemoteDelete
	ActionLocalMove    = synctypes.ActionLocalMove
	ActionRemoteMove   = synctypes.ActionRemoteMove
	ActionFolderCreate = synctypes.ActionFolderCreate
	ActionConflict     = synctypes.ActionConflict
	ActionUpdateSynced = synctypes.ActionUpdateSynced
	ActionCleanup      = synctypes.ActionCleanup
)

type (
	Baseline               = syncstore.Baseline
	ActionableFailure      = syncstore.ActionableFailure
	BaselineEntry          = syncstore.BaselineEntry
	ConflictRecord         = syncstore.ConflictRecord
	HeldDeleteRecord       = syncstore.HeldDeleteRecord
	ObservedItem           = syncstore.ObservedItem
	RecoveryCandidate      = syncstore.RecoveryCandidate
	RemoteStateRow         = syncstore.RemoteStateRow
	ScopeBlock             = syncstore.ScopeBlock
	ScopeStateApplyRequest = syncstore.ScopeStateApplyRequest
	ScopeStateRecord       = syncstore.ScopeStateRecord
	SyncFailureParams      = syncstore.SyncFailureParams
	SyncFailureRow         = syncstore.SyncFailureRow
)
