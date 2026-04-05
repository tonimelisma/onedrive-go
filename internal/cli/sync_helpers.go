package cli

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/sync"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// newSyncEngine creates a sync.Engine from a driveops.Session and resolved config.
// Validates syncDir and statePath, then builds the EngineConfig. Pass
// verifyDrive=true to enable drive-level hash verification (sync uses this;
// resolve does not need it since resolve only touches the conflict DB).
func newSyncEngine(
	ctx context.Context,
	session *driveops.Session,
	resolved *config.ResolvedDrive,
	verifyDrive bool,
	logger *slog.Logger,
) (*sync.Engine, error) {
	ecfg, err := buildSyncEngineConfig(session, resolved, verifyDrive, logger)
	if err != nil {
		return nil, err
	}

	engine, err := sync.NewEngine(ctx, ecfg)
	if err != nil {
		return nil, fmt.Errorf("create sync engine: %w", err)
	}

	return engine, nil
}

func buildSyncEngineConfig(
	session *driveops.Session,
	resolved *config.ResolvedDrive,
	verifyDrive bool,
	logger *slog.Logger,
) (*synctypes.EngineConfig, error) {
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

	ecfg := &synctypes.EngineConfig{
		DBPath:          dbPath,
		SyncRoot:        syncDir,
		DataDir:         config.DefaultDataDir(),
		DriveID:         session.DriveID,
		AccountEmail:    resolved.CanonicalID.Email(),
		RootItemID:      resolved.RootItemID,
		Fetcher:         session.Meta,
		SocketIOFetcher: session.Meta,
		Items:           session.Meta,
		Downloads:       session.Transfer,
		Uploads:         session.Transfer,
		FolderDelta:     session.Meta,
		RecursiveLister: session.Meta,
		PermChecker:     session.Meta,
		Logger:          logger,
		EnableWebsocket: resolved.Websocket,
		LocalFilter: synctypes.LocalFilterConfig{
			SkipDotfiles: resolved.SkipDotfiles,
			SkipSymlinks: resolved.SkipSymlinks,
			SkipDirs:     resolved.SkipDirs,
			SkipFiles:    resolved.SkipFiles,
		},
		LocalRules: synctypes.LocalObservationRules{
			RejectSharePointRootForms: resolved.CanonicalID.IsSharePoint(),
		},
		UseLocalTrash:      resolved.UseLocalTrash,
		TransferWorkers:    resolved.TransferWorkers,
		CheckWorkers:       resolved.CheckWorkers,
		BigDeleteThreshold: resolved.BigDeleteThreshold,
		MinFreeSpace:       minFree,
	}

	if verifyDrive {
		ecfg.DriveVerifier = session.Meta
	}

	return ecfg, nil
}
