package cli

import (
	"context"
	"errors"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/authstate"
	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

type statusAccountLiveOverlay struct {
	UserID      string
	DisplayName string
	AuthHealth  accountAuthHealth
	Degraded    *accountDegradedNotice
	LiveDrives  []statusLiveDrive
}

type liveDriveCatalogClient interface {
	Drives(context.Context) ([]graph.Drive, error)
	PrimaryDrive(context.Context) (*graph.Drive, error)
}

type liveDriveCatalogResult struct {
	AuthHealth accountAuthHealth
	Degraded   *accountDegradedNotice
	LiveDrives []statusLiveDrive
}

func loadStatusLiveOverlay(
	ctx context.Context,
	cc *CLIContext,
	catalog []accountCatalogEntry,
) map[string]statusAccountLiveOverlay {
	logger := cc.Logger
	recorder := newAuthProofRecorder(logger)
	overlays := make(map[string]statusAccountLiveOverlay, len(catalog))

	for i := range catalog {
		entry := catalog[i]
		if entry.SavedLoginState != savedLoginStateUsable || entry.RepresentativeTokenID.IsZero() {
			continue
		}

		tokenPath := config.DriveTokenPath(entry.RepresentativeTokenID)
		if tokenPath == "" {
			continue
		}

		ts, err := graph.TokenSourceFromPath(ctx, tokenPath, logger)
		if err != nil {
			switch {
			case errors.Is(err, graph.ErrNotLoggedIn):
				overlays[entry.Email] = statusAccountLiveOverlay{
					AuthHealth: authstate.RequiredHealth(authReasonMissingLogin),
				}
			default:
				overlays[entry.Email] = statusAccountLiveOverlay{
					AuthHealth: authstate.RequiredHealth(authReasonInvalidSavedLogin),
				}
			}
			continue
		}

		client, err := newGraphClientWithHTTP(cc.GraphBaseURL, cc.runtime().BootstrapMeta(), ts, logger)
		if err != nil {
			logger.Debug("status: creating graph client for live overlay", "account", entry.Email, "error", err)
			continue
		}
		attachAccountAuthProof(client, recorder, entry.Email, "status")

		user, err := client.Me(ctx)
		if err != nil {
			if errors.Is(err, graph.ErrUnauthorized) {
				overlays[entry.Email] = statusAccountLiveOverlay{
					AuthHealth: authstate.RequiredHealth(authReasonSyncAuthRejected),
				}
				if markErr := config.MarkAccountAuthRequired(config.DefaultDataDir(), entry.Email, authstate.ReasonSyncAuthRejected); markErr != nil {
					logger.Warn("status: persisting account auth requirement", "account", entry.Email, "error", markErr)
				}
			} else {
				logger.Warn("status: live account probe failed", "account", entry.Email, "error", err)
			}
			continue
		}

		if _, err := cc.reconcileGraphUser(entry.RepresentativeTokenID, user); err != nil {
			logger.Debug("status: reconcile graph user", "account", entry.Email, "error", err)
		}

		overlay := statusAccountLiveOverlay{
			UserID:      user.ID,
			DisplayName: user.DisplayName,
		}

		liveDrives := discoverLiveDriveCatalog(ctx, client, entry.Email, user.DisplayName, entry.DriveType, logger)
		overlay.LiveDrives = liveDrives.LiveDrives
		overlay.Degraded = liveDrives.Degraded
		overlay.AuthHealth = liveDrives.AuthHealth

		if overlay.AuthHealth.Reason == authReasonSyncAuthRejected {
			if markErr := config.MarkAccountAuthRequired(config.DefaultDataDir(), entry.Email, authstate.ReasonSyncAuthRejected); markErr != nil {
				logger.Warn("status: persisting account auth requirement after drive discovery unauthorized", "account", entry.Email, "error", markErr)
			}
		}

		overlays[entry.Email] = overlay
	}

	return overlays
}

func statusLiveDrives(drives []graph.Drive) []statusLiveDrive {
	if len(drives) == 0 {
		return nil
	}

	result := make([]statusLiveDrive, 0, len(drives))
	for i := range drives {
		result = append(result, statusLiveDrive{
			ID:         drives[i].ID.String(),
			Name:       drives[i].Name,
			DriveType:  drives[i].DriveType,
			QuotaUsed:  drives[i].QuotaUsed,
			QuotaTotal: drives[i].QuotaTotal,
		})
	}

	return result
}

func discoverLiveDriveCatalog(
	ctx context.Context,
	client liveDriveCatalogClient,
	email string,
	displayName string,
	driveType string,
	logger *slog.Logger,
) liveDriveCatalogResult {
	drives, err := client.Drives(ctx)
	if err == nil {
		return liveDriveCatalogResult{LiveDrives: statusLiveDrives(drives)}
	}

	if errors.Is(err, graph.ErrUnauthorized) {
		return liveDriveCatalogResult{AuthHealth: authstate.RequiredHealth(authReasonSyncAuthRejected)}
	}

	logger.Warn("degrading status live drive discovery after /me/drives failure",
		degradedDiscoveryLogAttrs(email, graphMeDrivesEndpoint, err)...,
	)

	notice := driveCatalogDegradedNotice(email, displayName, driveType)
	result := liveDriveCatalogResult{Degraded: &notice}
	primary, primaryErr := client.PrimaryDrive(ctx)
	if primaryErr == nil && primary != nil {
		result.LiveDrives = statusLiveDrives([]graph.Drive{*primary})
		result.Degraded.DriveType = primary.DriveType
		return result
	}

	if primaryErr != nil {
		logger.Warn("status: primary drive fallback unavailable after /me/drives failure",
			"account", email,
			"error", primaryErr,
		)
	}

	return result
}

func applyStatusLiveOverlay(accounts []statusAccount, overlays map[string]statusAccountLiveOverlay) {
	for i := range accounts {
		overlay, found := overlays[accounts[i].Email]
		if !found {
			continue
		}
		if overlay.UserID != "" {
			accounts[i].UserID = overlay.UserID
		}
		if overlay.DisplayName != "" {
			accounts[i].DisplayName = overlay.DisplayName
		}
		if overlay.AuthHealth.State != "" {
			accounts[i].AuthState = overlay.AuthHealth.State
			accounts[i].AuthReason = string(overlay.AuthHealth.Reason)
			accounts[i].AuthAction = overlay.AuthHealth.Action
		}
		if overlay.Degraded != nil {
			accounts[i].DegradedReason = overlay.Degraded.Reason
			accounts[i].DegradedAction = overlay.Degraded.Action
		}
		accounts[i].LiveDrives = overlay.LiveDrives
	}
}
