package cli

import (
	"context"
	"errors"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/authstate"
	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

type statusAccountLiveOverlay struct {
	DisplayName string
	AuthHealth  accountAuthHealth
	Storage     map[string]statusStorage
}

type configuredStatusDriveClient interface {
	Me(context.Context) (*graph.User, error)
	PrimaryDrive(context.Context) (*graph.Drive, error)
	Drive(context.Context, driveid.ID) (*graph.Drive, error)
}

func loadStatusLiveOverlay(
	ctx context.Context,
	cc *CLIContext,
	snapshot accountViewSnapshot,
) map[string]statusAccountLiveOverlay {
	logger := cc.Logger
	recorder := newAuthProofRecorder(logger)
	overlays := make(map[string]statusAccountLiveOverlay, len(snapshot.Accounts))

	for i := range snapshot.Accounts {
		entry := snapshot.Accounts[i]
		if entry.SavedLoginReason != "" || entry.RepresentativeTokenID.IsZero() {
			continue
		}

		tokenPath := config.DriveTokenPath(entry.RepresentativeTokenID)
		if tokenPath == "" {
			continue
		}

		ts, err := graph.TokenSourceFromPath(ctx, tokenPath, logger)
		if err != nil {
			overlays[entry.Email] = statusLiveAuthOverlay(err)
			continue
		}

		client, err := newGraphClientWithHTTP(cc.GraphBaseURL, cc.runtime().BootstrapMeta(), ts, logger)
		if err != nil {
			logger.Debug("status: creating graph client for live overlay", "account", entry.Email, "error", err)
			continue
		}
		attachAccountAuthProof(client, recorder, entry.Email, "status")

		overlay, ok := statusConfiguredDriveOverlay(ctx, client, snapshot, entry, logger)
		if ok {
			overlays[entry.Email] = overlay
		}
	}

	return overlays
}

func statusLiveAuthOverlay(err error) statusAccountLiveOverlay {
	if errors.Is(err, graph.ErrNotLoggedIn) {
		return statusAccountLiveOverlay{
			AuthHealth: authstate.RequiredHealth(authReasonMissingLogin),
		}
	}

	return statusAccountLiveOverlay{
		AuthHealth: authstate.RequiredHealth(authReasonInvalidSavedLogin),
	}
}

func statusConfiguredDriveOverlay(
	ctx context.Context,
	client configuredStatusDriveClient,
	snapshot accountViewSnapshot,
	entry accountView,
	logger *slog.Logger,
) (statusAccountLiveOverlay, bool) {
	user, err := client.Me(ctx)
	if err != nil {
		if errors.Is(err, graph.ErrUnauthorized) {
			if markErr := config.MarkAccountAuthRequired(config.DefaultDataDir(), entry.Email, authstate.ReasonSyncAuthRejected); markErr != nil {
				logger.Warn("status: persisting account auth requirement", "account", entry.Email, "error", markErr)
			}
			return statusAccountLiveOverlay{
				AuthHealth: authstate.RequiredHealth(authReasonSyncAuthRejected),
			}, true
		}
		logger.Warn("status: live account probe failed", "account", entry.Email, "error", err)
		return statusAccountLiveOverlay{}, false
	}

	overlay := statusAccountLiveOverlay{
		DisplayName: user.DisplayName,
		AuthHealth:  authstate.ReadyHealth(),
		Storage:     make(map[string]statusStorage),
	}

	for _, cid := range entry.ConfiguredDriveIDs {
		drive, err := fetchConfiguredStatusDrive(ctx, client, snapshot.Stored, cid)
		if err != nil {
			if errors.Is(err, graph.ErrUnauthorized) {
				if markErr := config.MarkAccountAuthRequired(config.DefaultDataDir(), entry.Email, authstate.ReasonSyncAuthRejected); markErr != nil {
					logger.Warn("status: persisting account auth requirement after configured drive probe", "account", entry.Email, "error", markErr)
				}
				overlay.AuthHealth = authstate.RequiredHealth(authReasonSyncAuthRejected)
				return overlay, true
			}
			logger.Debug("status: configured drive live storage unavailable",
				"account", entry.Email,
				"drive", cid.String(),
				"error", err,
			)
			continue
		}
		if drive != nil {
			overlay.Storage[cid.String()] = statusStorageFromGraphDrive(*drive)
		}
	}

	return overlay, true
}

func fetchConfiguredStatusDrive(
	ctx context.Context,
	client configuredStatusDriveClient,
	stored *config.Catalog,
	cid driveid.CanonicalID,
) (*graph.Drive, error) {
	if cid.IsPersonal() || cid.IsBusiness() {
		return client.PrimaryDrive(ctx)
	}
	if stored == nil {
		return nil, nil
	}

	record, found := stored.DriveByCanonicalID(cid)
	if !found || record.RemoteDriveID == "" {
		return nil, nil
	}

	return client.Drive(ctx, driveid.New(record.RemoteDriveID))
}

func statusStorageFromGraphDrive(drive graph.Drive) statusStorage {
	storage := statusStorage{
		UsedBytes:  drive.QuotaUsed,
		TotalBytes: drive.QuotaTotal,
		Used:       formatSize(drive.QuotaUsed),
	}
	if drive.QuotaTotal > 0 {
		storage.Total = formatSize(drive.QuotaTotal)
	}
	return storage
}

func applyStatusLiveOverlay(accounts []statusAccount, overlays map[string]statusAccountLiveOverlay) {
	for i := range accounts {
		overlay, found := overlays[accounts[i].Email]
		if !found {
			continue
		}
		if overlay.DisplayName != "" {
			accounts[i].DisplayName = overlay.DisplayName
		}
		if overlay.AuthHealth.State != "" {
			accounts[i].AuthState = overlay.AuthHealth.State
			accounts[i].AuthReason = string(overlay.AuthHealth.Reason)
			accounts[i].AuthAction = overlay.AuthHealth.Action
			accounts[i].SignInRequired = nil
			if accounts[i].AuthState == authStateAuthenticationNeeded {
				setAccountSignInRequired(&accounts[i])
			}
		}
		if len(overlay.Storage) > 0 {
			applyStatusStorageOverlay(accounts[i].Drives, overlay.Storage)
		}
	}
}

func applyStatusStorageOverlay(drives []statusDrive, storage map[string]statusStorage) {
	for i := range drives {
		if value, ok := storage[drives[i].InternalID]; ok {
			valueCopy := value
			drives[i].Storage = &valueCopy
		}
		applyStatusStorageOverlay(drives[i].SharedFolders, storage)
	}
}
