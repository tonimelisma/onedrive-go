package sync

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/perf"
)

// DriveEngineOptions carries the small set of construction decisions that are
// not owned by the resolved drive/session pair itself.
type DriveEngineOptions struct {
	Logger        *slog.Logger
	PerfCollector *perf.Collector
	VerifyDrive   bool
}

// NewDriveEngine constructs an Engine directly from the authenticated drive
// session and resolved drive config used by production entrypoints.
func NewDriveEngine(
	ctx context.Context,
	session *driveops.Session,
	resolved *config.ResolvedDrive,
	opts DriveEngineOptions,
) (*Engine, error) {
	cfg, err := newEngineConfigForDrive(session, resolved, opts)
	if err != nil {
		return nil, err
	}

	return newEngine(ctx, cfg)
}

func newEngineConfigForDrive(
	session *driveops.Session,
	resolved *config.ResolvedDrive,
	opts DriveEngineOptions,
) (*engineInputs, error) {
	if session == nil {
		return nil, fmt.Errorf("sync: session is required")
	}

	if resolved == nil {
		return nil, fmt.Errorf("sync: resolved drive is required")
	}

	logger := opts.Logger
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

	ecfg := &engineInputs{
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
		PerfCollector:   opts.PerfCollector,
	}

	if opts.VerifyDrive {
		ecfg.DriveVerifier = session.Meta
	}

	return ecfg, nil
}
