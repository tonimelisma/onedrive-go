package cli

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/sync"
)

// newSyncEngine creates a sync.Engine from a driveops.Session and resolved config.
// Pass verifyDrive=true to enable drive-level hash verification (sync uses
// this; resolve does not need it since resolve only touches the conflict DB).
func newSyncEngine(
	ctx context.Context,
	session *driveops.Session,
	resolved *config.ResolvedDrive,
	verifyDrive bool,
	logger *slog.Logger,
) (*sync.Engine, error) {
	engine, err := sync.NewDriveEngine(ctx, session, resolved, sync.DriveEngineOptions{
		Logger:      logger,
		VerifyDrive: verifyDrive,
	})
	if err != nil {
		return nil, fmt.Errorf("create sync engine: %w", err)
	}

	return engine, nil
}
