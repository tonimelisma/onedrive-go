package sync

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/perf"
)

// NewDriveEngine constructs an Engine directly from the authenticated drive
// session and resolved drive config used by production entrypoints.
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

	cfg := &engineInputs{
		DBPath:                 dbPath,
		SyncRoot:               syncDir,
		DataDir:                config.DefaultDataDir(),
		DriveID:                session.DriveID,
		DriveType:              resolved.CanonicalID.DriveType(),
		AccountEmail:           resolved.CanonicalID.Email(),
		RootItemID:             resolved.RootItemID,
		SharedRootDeltaCapable: resolved.SharedRootDeltaCapable,
		Fetcher:                session.Meta,
		SocketIOFetcher:        session.Meta,
		Items:                  session.Meta,
		Downloads:              session.Transfer,
		Uploads:                session.Transfer,
		PathConvergenceFactory: session,
		FolderDelta:            session.Meta,
		RecursiveLister:        session.Meta,
		PermChecker:            session.Meta,
		Logger:                 logger,
		EnableWebsocket:        resolved.Websocket,
		LocalFilter:            LocalFilterConfig{},
		LocalRules: LocalObservationRules{
			RejectSharePointRootForms: resolved.CanonicalID.IsSharePoint(),
		},
		TransferWorkers: resolved.TransferWorkers,
		CheckWorkers:    resolved.CheckWorkers,
		MinFreeSpace:    minFree,
		PerfCollector:   perfCollector,
	}

	if verifyDrive {
		cfg.DriveVerifier = session.Meta
	}

	return newEngine(ctx, cfg)
}
