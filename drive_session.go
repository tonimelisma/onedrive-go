package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// DriveSession holds authenticated clients and the resolved drive identity
// for a single drive. Replaces the ad-hoc clientAndDrive() 4-tuple with a
// proper type that includes both metadata and transfer clients.
type DriveSession struct {
	Client   *graph.Client // metadata ops (30s timeout)
	Transfer *graph.Client // uploads/downloads (no timeout)
	DriveID  driveid.ID
	Resolved *config.ResolvedDrive
}

// NewDriveSession creates a DriveSession from resolved config. It resolves the
// token path (supporting shared drives via cfg), loads the token, and creates
// both metadata and transfer clients. DriveID must be pre-resolved (via token
// meta or config); returns an error if it is zero.
func NewDriveSession(ctx context.Context, resolved *config.ResolvedDrive, cfg *config.Config, logger *slog.Logger) (*DriveSession, error) {
	tokenPath := config.DriveTokenPath(resolved.CanonicalID, cfg)
	if tokenPath == "" {
		return nil, fmt.Errorf("cannot determine token path for drive %q", resolved.CanonicalID)
	}

	ts, err := graph.TokenSourceFromPath(ctx, tokenPath, logger)
	if err != nil {
		if errors.Is(err, graph.ErrNotLoggedIn) {
			return nil, fmt.Errorf("not logged in — run 'onedrive-go login' first")
		}

		return nil, err
	}

	client := newGraphClient(ts, logger)
	transfer := newTransferGraphClient(ts, logger)

	driveID := resolved.DriveID
	if driveID.IsZero() {
		return nil, fmt.Errorf("drive ID not resolved for %s — re-run 'onedrive-go login'", resolved.CanonicalID)
	}

	logger.Debug("using drive ID", "drive_id", driveID.String())

	return &DriveSession{
		Client:   client,
		Transfer: transfer,
		DriveID:  driveID,
		Resolved: resolved,
	}, nil
}
