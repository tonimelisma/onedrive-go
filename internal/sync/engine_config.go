package sync

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/perf"
)

// EngineMountConfig carries the non-client runtime facts needed to construct an
// engine for one mounted content root.
type EngineMountConfig struct {
	DBPath                    string
	SyncRoot                  string
	DataDir                   string
	DriveID                   driveid.ID
	DriveType                 string
	AccountEmail              string
	RootItemID                string
	RootedSubtreeDeltaCapable bool
	EnableWebsocket           bool
	LocalFilter               LocalFilterConfig
	LocalRules                LocalObservationRules
	TransferWorkers           int
	CheckWorkers              int
	MinFreeSpace              int64
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

	cfg := &engineInputs{
		DBPath:                    mountCfg.DBPath,
		SyncRoot:                  mountCfg.SyncRoot,
		DataDir:                   mountCfg.DataDir,
		DriveID:                   mountCfg.DriveID,
		DriveType:                 mountCfg.DriveType,
		AccountEmail:              mountCfg.AccountEmail,
		RootItemID:                mountCfg.RootItemID,
		RootedSubtreeDeltaCapable: mountCfg.RootedSubtreeDeltaCapable,
		Fetcher:                   session.Meta,
		SocketIOFetcher:           session.Meta,
		Items:                     session.Meta,
		Downloads:                 session.Transfer,
		Uploads:                   session.Transfer,
		PathConvergenceFactory:    session,
		FolderDelta:               session.Meta,
		RecursiveLister:           session.Meta,
		PermChecker:               session.Meta,
		Logger:                    logger,
		EnableWebsocket:           mountCfg.EnableWebsocket,
		LocalFilter:               mountCfg.LocalFilter,
		LocalRules:                mountCfg.LocalRules,
		TransferWorkers:           mountCfg.TransferWorkers,
		CheckWorkers:              mountCfg.CheckWorkers,
		MinFreeSpace:              mountCfg.MinFreeSpace,
		PerfCollector:             perfCollector,
	}

	if verifyDrive {
		cfg.DriveVerifier = session.Meta
	}

	return newEngine(ctx, cfg)
}

// NewDriveEngine constructs an Engine from the authenticated drive session and
// resolved drive config for transitional resolved-drive callers.
func NewDriveEngine(
	ctx context.Context,
	session *driveops.Session,
	resolved *config.ResolvedDrive,
	logger *slog.Logger,
	perfCollector *perf.Collector,
	verifyDrive bool,
) (*Engine, error) {
	if session == nil {
		return nil, fmt.Errorf("sync: session is required")
	}

	if resolved == nil {
		return nil, fmt.Errorf("sync: resolved drive is required")
	}

	if logger == nil {
		logger = slog.Default()
	}

	syncDir := resolved.SyncDir
	if syncDir == "" {
		return nil, fmt.Errorf("sync_dir not configured — set it in the config file or add a drive with 'onedrive-go drive add'")
	}

	dbPath := resolved.StatePath()
	if dbPath == "" {
		return nil, fmt.Errorf("cannot determine state DB path for drive %q", resolved.CanonicalID)
	}

	minFree, err := config.ParseSize(resolved.MinFreeSpace)
	if err != nil {
		return nil, fmt.Errorf("invalid min_free_space %q: %w", resolved.MinFreeSpace, err)
	}

	return NewMountEngine(ctx, session, &EngineMountConfig{
		DBPath:                    dbPath,
		SyncRoot:                  syncDir,
		DataDir:                   config.DefaultDataDir(),
		DriveID:                   resolved.DriveID,
		DriveType:                 resolved.CanonicalID.DriveType(),
		AccountEmail:              resolved.CanonicalID.Email(),
		RootItemID:                resolved.RootItemID,
		RootedSubtreeDeltaCapable: resolved.SharedRootDeltaCapable,
		EnableWebsocket:           resolved.Websocket,
		LocalFilter:               LocalFilterConfig{},
		LocalRules: LocalObservationRules{
			RejectSharePointRootForms: resolved.CanonicalID.IsSharePoint(),
		},
		TransferWorkers: resolved.TransferWorkers,
		CheckWorkers:    resolved.CheckWorkers,
		MinFreeSpace:    minFree,
	}, logger, perfCollector, verifyDrive)
}
