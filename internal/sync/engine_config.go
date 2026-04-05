package sync

import (
	"fmt"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/syncscope"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// BuildEngineConfig is the single authority for translating an authenticated
// drive session plus resolved config into an EngineConfig.
func BuildEngineConfig(
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
		DriveType:       resolved.CanonicalID.DriveType(),
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
		SyncScope: syncscope.Config{
			SyncPaths:    append([]string(nil), resolved.SyncPaths...),
			IgnoreMarker: resolved.IgnoreMarker,
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
