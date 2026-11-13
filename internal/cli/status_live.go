package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/authstate"
	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/panicguard"
	"github.com/tonimelisma/onedrive-go/internal/retry"
)

// statusLiveBudget is how long status waits for OneDrive's live details,
// for every account together (R-6.8.28). The details only decorate what
// status knows locally, so a OneDrive slower than this costs the decoration,
// never the command: without a budget, a GET /me that answered each attempt
// with a server error after about 21 s kept status quiet for 177 s.
const statusLiveBudget = 10 * time.Second

// statusLiveNoteFormat is the stderr line for an account whose live details
// did not all arrive: the account, then why, in the user's words. stderr keeps
// --json stdout one document.
const statusLiveNoteFormat = "Note: showing what is known locally for %s: %s.\n"

// statusLiveInternalErrorReason is the note's reason for an account whose
// live details a panic cut short. The log line names the bug.
const statusLiveInternalErrorReason = "an internal error stopped the request"

// errStatusLiveBudgetSpent is the cause the overlay context ends with when
// the budget runs out, which tells a request the budget cut short from one
// that failed on its own or that a stopped command canceled.
var errStatusLiveBudgetSpent = errors.New("status: live details budget spent")

type statusAccountLiveOverlay struct {
	DisplayName string
	AuthHealth  accountAuthHealth
	Storage     map[string]statusStorage
	// Unanswered says why some of the account's live details are missing, in
	// the user's words; empty when every answer arrived or the user stopped
	// the command.
	Unanswered string
}

type configuredStatusDriveClient interface {
	Me(context.Context) (*graph.User, error)
	PrimaryDrive(context.Context) (*graph.Drive, error)
	Drive(context.Context, driveid.ID) (*graph.Drive, error)
}

// liveOverlayBudgetContext is ctx under the live details' budget: it ends
// with errStatusLiveBudgetSpent as its cause once statusLiveBudget has passed.
// A test supplies a context it ends itself, so no test waits for the clock or
// races it.
func liveOverlayBudgetContext(ctx context.Context, cc *CLIContext) (context.Context, context.CancelFunc) {
	if cc.seams.statusLiveBudgetContext != nil {
		return cc.seams.statusLiveBudgetContext(ctx)
	}

	return context.WithTimeoutCause(ctx, statusLiveBudget, errStatusLiveBudgetSpent)
}

// loadStatusLiveOverlay asks OneDrive for every account's live details at the
// same time, under one budget for all of them, and waits for every request it
// started: each ends with its answer or, at the latest, when the budget does.
// An account the budget cut short keeps what did arrive and says why the rest
// is missing.
func loadStatusLiveOverlay(
	ctx context.Context,
	cc *CLIContext,
	snapshot accountViewSnapshot,
) map[string]statusAccountLiveOverlay {
	logger := cc.Logger
	recorder := newAuthProofRecorder(logger)
	runtime := cc.runtime()

	askCtx, cancel := liveOverlayBudgetContext(ctx, cc)
	defer cancel()

	results := make([]statusAccountLiveOverlay, len(snapshot.Accounts))
	hasOverlay := make([]bool, len(snapshot.Accounts))
	var wg sync.WaitGroup

	for i := range snapshot.Accounts {
		entry := &snapshot.Accounts[i]
		if entry.SavedLoginReason != "" || entry.RepresentativeTokenID.IsZero() {
			continue
		}

		tokenPath := config.DriveTokenPath(entry.RepresentativeTokenID)
		if tokenPath == "" {
			continue
		}

		// The command's context, not askCtx: the runtime keeps the token
		// source with the context it was built under and refreshes with it.
		client, err := runtime.BootstrapClient(ctx, tokenPath)
		if err != nil {
			logger.Debug("status: loading sign-in for live overlay", "account", entry.Email, "error", err)
			results[i], hasOverlay[i] = statusLiveAuthOverlay(err), true
			continue
		}
		attachAccountAuthProof(client, recorder, entry.Email, "status")

		hasOverlay[i] = true
		wg.Go(func() {
			// A panic leaves this account without its live details, as a
			// failed profile request does, while the other accounts are
			// asked (R-6.3.10).
			defer func() {
				if r := recover(); r != nil {
					//nolint:errcheck // The note is the account's report; the error is only logged.
					panicguard.Recovered(logger, "while asking OneDrive for the live details of "+entry.Email, r)
					results[i] = statusAccountLiveOverlay{Unanswered: statusLiveInternalErrorReason}
				}
			}()

			overlay, askErr := statusConfiguredDriveOverlay(askCtx, client, snapshot, entry, logger)
			if askErr != nil {
				overlay.Unanswered = statusLiveUnansweredReason(ctx, askCtx, askErr)
				logStatusLiveFailure(ctx, logger, entry.Email, overlay.Unanswered, askErr)
			}
			results[i] = overlay
		})
	}

	wg.Wait()

	overlays := make(map[string]statusAccountLiveOverlay, len(snapshot.Accounts))
	for i := range snapshot.Accounts {
		if hasOverlay[i] {
			overlays[snapshot.Accounts[i].Email] = results[i]
		}
	}

	return overlays
}

// statusLiveUnansweredReason says in the user's words why OneDrive's live
// details for an account are missing, or "" when the user stopped the
// command: a canceled request is expected then, not news.
func statusLiveUnansweredReason(ctx, askCtx context.Context, err error) string {
	if ctx.Err() != nil {
		return ""
	}
	// The context's cause, not the error chain: a response body cut off by
	// the budget reports the context's error without its cause.
	if errors.Is(context.Cause(askCtx), errStatusLiveBudgetSpent) {
		return "OneDrive did not answer within " + retry.HumanDuration(statusLiveBudget)
	}

	var graphErr *graph.GraphError
	switch {
	case errors.Is(err, graph.ErrThrottled):
		return "OneDrive is limiting requests"
	case errors.Is(err, graph.ErrServerError):
		return "OneDrive had a temporary server problem"
	case errors.As(err, &graphErr) && graphErr.Message != "":
		// The note ends the sentence, so OneDrive's own full stop goes.
		return "OneDrive refused the request: " + strings.TrimRight(graphErr.Message, ".")
	default:
		return plainErrorReason(err)
	}
}

// logStatusLiveFailure records the failure behind a status note: at Info,
// so --verbose shows the error chain the note puts in plain words, and at
// Debug when there is no note because the user stopped the command.
func logStatusLiveFailure(ctx context.Context, logger *slog.Logger, email, reason string, err error) {
	level := slog.LevelInfo
	if reason == "" {
		level = slog.LevelDebug
	}

	logger.Log(ctx, level, "status: live account details unavailable",
		"account", email, "reason", reason, "error", err)
}

// printStatusLiveNotes writes one stderr line per account whose live details
// did not all arrive, in the order status lists the accounts.
func printStatusLiveNotes(cc *CLIContext, accounts []statusAccount, overlays map[string]statusAccountLiveOverlay) {
	for i := range accounts {
		if overlay, found := overlays[accounts[i].Email]; found && overlay.Unanswered != "" {
			cc.Statusf(statusLiveNoteFormat, accounts[i].Email, overlay.Unanswered)
		}
	}
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

// statusConfiguredDriveOverlay asks for one account's profile and its
// configured drives' storage. A sign-in failure is an answer: the account is
// marked as needing sign-in, and no error is returned. The error it returns
// is what left the overlay incomplete: a failed profile request, or a storage
// request ctx ended (later drives would fail the same way). Any other storage
// failure leaves that drive's storage out, as before (a shared drive need not
// show its owner's quota).
func statusConfiguredDriveOverlay(
	ctx context.Context,
	client configuredStatusDriveClient,
	snapshot accountViewSnapshot,
	entry *accountView,
	logger *slog.Logger,
) (statusAccountLiveOverlay, error) {
	user, err := client.Me(ctx)
	if err != nil {
		if errors.Is(err, graph.ErrUnauthorized) {
			return statusAccountLiveOverlay{
				AuthHealth: markStatusAccountSignInRequired(entry.Email, err, logger),
			}, nil
		}

		return statusAccountLiveOverlay{}, fmt.Errorf("ask OneDrive for the account profile: %w", err)
	}

	overlay := statusAccountLiveOverlay{
		DisplayName: user.DisplayName,
		AuthHealth:  authstate.ReadyHealth(),
		Storage:     make(map[string]statusStorage),
	}

	for _, cid := range entry.ConfiguredDriveIDs {
		drive, found, err := fetchConfiguredStatusDrive(ctx, client, snapshot.Stored, cid)
		if err != nil {
			if errors.Is(err, graph.ErrUnauthorized) {
				overlay.AuthHealth = markStatusAccountSignInRequired(entry.Email, err, logger)
				return overlay, nil
			}
			if ctx.Err() != nil {
				return overlay, fmt.Errorf("ask OneDrive for drive storage: %w", err)
			}
			logger.Debug("status: configured drive live storage unavailable",
				"account", entry.Email,
				"drive", cid.String(),
				"error", err,
			)
			continue
		}
		if found {
			overlay.Storage[cid.String()] = statusStorageFromGraphDrive(*drive)
		}
	}

	return overlay, nil
}

// markStatusAccountSignInRequired records that OneDrive rejected the
// account's saved login and returns the health status shows for it.
func markStatusAccountSignInRequired(email string, err error, logger *slog.Logger) accountAuthHealth {
	reason := unauthorizedAuthReason(err)
	if markErr := config.MarkAccountAuthRequired(config.DefaultDataDir(), email, reason); markErr != nil {
		logger.Warn("status: persisting account auth requirement", "account", email, "error", markErr)
	}

	return authstate.RequiredHealth(reason)
}

func fetchConfiguredStatusDrive(
	ctx context.Context,
	client configuredStatusDriveClient,
	stored *config.Catalog,
	cid driveid.CanonicalID,
) (*graph.Drive, bool, error) {
	if cid.IsPersonal() || cid.IsBusiness() {
		drive, err := client.PrimaryDrive(ctx)
		if err != nil {
			return nil, false, fmt.Errorf("fetch primary drive: %w", err)
		}
		return drive, drive != nil, nil
	}
	if stored == nil {
		return nil, false, nil
	}

	record, found := stored.DriveByCanonicalID(cid)
	if !found || record.RemoteDriveID == "" {
		return nil, false, nil
	}

	drive, err := client.Drive(ctx, driveid.New(record.RemoteDriveID))
	if err != nil {
		return nil, false, fmt.Errorf("fetch configured drive: %w", err)
	}
	return drive, drive != nil, nil
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

// unauthorizedAuthReason distinguishes a saved login Microsoft has expired or
// revoked from a request OneDrive rejected with a still-valid login.
func unauthorizedAuthReason(err error) authstate.Reason {
	if errors.Is(err, graph.ErrLoginExpired) {
		return authReasonSavedLoginExpired
	}

	return authReasonSyncAuthRejected
}
