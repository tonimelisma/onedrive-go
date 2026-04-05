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
	ecfg, err := sync.BuildEngineConfig(session, resolved, verifyDrive, logger)
	if err != nil {
		return nil, fmt.Errorf("build sync engine config: %w", err)
	}

	engine, err := sync.NewEngine(ctx, ecfg)
	if err != nil {
		return nil, fmt.Errorf("create sync engine: %w", err)
	}

	return engine, nil
}
