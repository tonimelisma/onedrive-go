package main

import (
	"fmt"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/sync"
)

// newSyncEngine creates a sync.Engine from a driveops.Session and resolved config.
// Validates syncDir and statePath, then builds the EngineConfig. Pass
// verifyDrive=true to enable drive-level hash verification (sync uses this;
// resolve does not need it since resolve only touches the conflict DB).
func newSyncEngine(session *driveops.Session, resolved *config.ResolvedDrive, verifyDrive bool, logger *slog.Logger) (*sync.Engine, error) {
	syncDir := resolved.SyncDir
	if syncDir == "" {
		return nil, fmt.Errorf("sync_dir not configured — set it in the config file or add a drive with 'onedrive-go drive add'")
	}

	dbPath := resolved.StatePath()
	if dbPath == "" {
		return nil, fmt.Errorf("cannot determine state DB path for drive %q", resolved.CanonicalID)
	}

	ecfg := &sync.EngineConfig{
		DBPath:          dbPath,
		SyncRoot:        syncDir,
		DataDir:         config.DefaultDataDir(),
		DriveID:         session.DriveID,
		Fetcher:         session.Meta,
		Items:           session.Meta,
		Downloads:       session.Transfer,
		Uploads:         session.Transfer,
		Logger:          logger,
		UseLocalTrash:   resolved.UseLocalTrash,
		TransferWorkers: resolved.TransferWorkers,
		CheckWorkers:    resolved.CheckWorkers,
	}

	if verifyDrive {
		ecfg.DriveVerifier = session.Meta
	}

	return sync.NewEngine(ecfg)
}
