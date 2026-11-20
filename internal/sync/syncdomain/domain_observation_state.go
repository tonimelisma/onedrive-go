package syncdomain

import (
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

type ObservationState struct {
	ContentDriveID driveid.ID
	Cursor         string
	// NextFullRemoteRefreshAt is when the next full remote refresh is due,
	// as StoredTimeNano encodes it; zero means one is due now.
	NextFullRemoteRefreshAt  int64
	LocalTruthComplete       bool
	LocalTruthRecoveryReason string
	// SyncRootIdentity is the fingerprint of the local folder this state was
	// built against, recorded with the first committed local snapshot. Nil
	// until then, and again after an explicit rebind.
	SyncRootIdentity *synctree.RootIdentity
	// SyncRootRebind records an explicit `drive rebind-root`: the next start
	// may bind whatever folder is at the sync path even though the baseline
	// is not empty. Cleared by the bind.
	SyncRootRebind bool
	// UndecidedRefreshStreak counts consecutive full refreshes that kept an
	// orphan they could not confirm; it sets how soon the next one runs.
	UndecidedRefreshStreak int
}

const (
	LocalTruthRecoveryDroppedEvents  = "dropped_local_events"
	LocalTruthRecoveryWatcherError   = "watcher_error"
	LocalTruthRecoveryFullScanFailed = "full_local_scan_failed"
)
