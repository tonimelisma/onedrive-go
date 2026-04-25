package sync

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/perf"
)

// EngineMountConfig carries the non-client runtime facts needed to construct an
// engine for one mounted content root.
type EngineMountConfig struct {
	DBPath                 string
	SyncRoot               string
	DataDir                string
	DriveID                driveid.ID
	DriveType              string
	AccountEmail           string
	RemoteRootItemID       string
	RemoteRootDeltaCapable bool
	EnableWebsocket        bool
	LocalFilter            LocalFilterConfig
	LocalRules             LocalObservationRules
	ManagedRootEvents      ManagedRootEventSink
	TransferWorkers        int
	CheckWorkers           int
	MinFreeSpace           int64
}

// NewMountEngine constructs an Engine directly from the authenticated session
// capabilities plus mount-owned runtime config.
func NewMountEngine(
	ctx context.Context,
	session *driveops.Session,
	mountCfg *EngineMountConfig,
	logger *slog.Logger,
	perfCollector *perf.Collector,
	verifyDrive bool,
) (*Engine, error) {
	if session == nil {
		return nil, fmt.Errorf("sync: session is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if mountCfg == nil {
		return nil, fmt.Errorf("sync: mount config is required")
	}
	if mountCfg.SyncRoot == "" {
		return nil, fmt.Errorf("sync: sync root is required")
	}
	if mountCfg.DBPath == "" {
		return nil, fmt.Errorf("sync: state DB path is required")
	}
	if mountCfg.DriveID.IsZero() {
		return nil, fmt.Errorf("sync: drive ID is required")
	}

	pathConvergence := driveops.NewMountSession(session, mountCfg.RemoteRootItemID)

	cfg := &engineInputs{
		DBPath:                 mountCfg.DBPath,
		SyncRoot:               mountCfg.SyncRoot,
		DataDir:                mountCfg.DataDir,
		DriveID:                mountCfg.DriveID,
		DriveType:              mountCfg.DriveType,
		AccountEmail:           mountCfg.AccountEmail,
		RemoteRootItemID:       mountCfg.RemoteRootItemID,
		RemoteRootDeltaCapable: mountCfg.RemoteRootDeltaCapable,
		Fetcher:                session.Meta,
		SocketIOFetcher:        session.Meta,
		Items:                  session.Meta,
		Downloads:              session.Transfer,
		Uploads:                session.Transfer,
		PathConvergence:        pathConvergence,
		FolderDelta:            session.Meta,
		RecursiveLister:        session.Meta,
		PermChecker:            session.Meta,
		Logger:                 logger,
		EnableWebsocket:        mountCfg.EnableWebsocket,
		LocalFilter:            mountCfg.LocalFilter,
		LocalRules:             mountCfg.LocalRules,
		ManagedRootEvents:      mountCfg.ManagedRootEvents,
		TransferWorkers:        mountCfg.TransferWorkers,
		CheckWorkers:           mountCfg.CheckWorkers,
		MinFreeSpace:           mountCfg.MinFreeSpace,
		PerfCollector:          perfCollector,
	}

	if verifyDrive {
		cfg.DriveVerifier = session.Meta
	}

	return newEngine(ctx, cfg)
}
