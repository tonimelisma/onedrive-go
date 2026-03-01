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
	Client      *graph.Client     // metadata ops (30s timeout)
	Transfer    *graph.Client     // uploads/downloads (no timeout)
	TokenSource graph.TokenSource // for callers that need raw token
	DriveID     driveid.ID
	Resolved    *config.ResolvedDrive
}

// NewDriveSession creates a DriveSession from resolved config. It resolves the
// token path (supporting shared drives via cfg), loads the token, creates both
// metadata and transfer clients, and discovers the drive ID if not configured.
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
		drives, discoverErr := client.Drives(ctx)
		if discoverErr != nil {
			return nil, fmt.Errorf("discovering drive: %w", discoverErr)
		}

		if len(drives) == 0 {
			return nil, fmt.Errorf("no drives found for this account")
		}

		driveID = drives[0].ID
		logger.Debug("discovered primary drive", "drive_id", driveID.String())
	} else {
		logger.Debug("using configured drive ID", "drive_id", driveID.String())
	}

	return &DriveSession{
		Client:      client,
		Transfer:    transfer,
		TokenSource: ts,
		DriveID:     driveID,
		Resolved:    resolved,
	}, nil
}
