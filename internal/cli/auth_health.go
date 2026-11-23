package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"sync"

	"github.com/tonimelisma/onedrive-go/internal/authstate"
	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

const (
	authStateReady                 = authstate.StateReady
	authStateAuthenticationNeeded  = authstate.StateAuthenticationRequired
	authReasonMissingLogin         = authstate.ReasonMissingLogin
	authReasonInvalidSavedLogin    = authstate.ReasonInvalidSavedLogin
	authReasonSavedLoginUnreadable = authstate.ReasonSavedLoginUnreadable
	authReasonSyncAuthRejected     = authstate.ReasonSyncAuthRejected
	authReasonSavedLoginExpired    = authstate.ReasonSavedLoginExpired
)

type accountAuthHealth = authstate.Health

type accountAuthRequirement struct {
	Email       string           `json:"email"`
	DisplayName string           `json:"display_name,omitempty"`
	DriveType   string           `json:"drive_type,omitempty"`
	Reason      authstate.Reason `json:"reason"`
	Action      string           `json:"action,omitempty"`
	StateDBs    int              `json:"state_dbs,omitempty"`
}

// authProofRecorder ends an account's sign-in requirement once a request made
// with its saved login succeeded. Accounts are named by canonical ID, the
// owner of the token the request used: a personal and a work or school
// account can share an address, and a proof of one says nothing about the
// other.
type authProofRecorder struct {
	logger  *slog.Logger
	mu      sync.Mutex // guards cleared so one successful proof clears scopes once per account
	cleared map[driveid.CanonicalID]bool
}

func newAuthProofRecorder(logger *slog.Logger) *authProofRecorder {
	return &authProofRecorder{
		logger:  logger,
		cleared: make(map[driveid.CanonicalID]bool),
	}
}

func (r *authProofRecorder) Hook(account driveid.CanonicalID, proofSource string) func(context.Context) {
	return func(ctx context.Context) {
		r.recordSuccess(ctx, account, proofSource)
	}
}

func (r *authProofRecorder) recordSuccess(ctx context.Context, account driveid.CanonicalID, proofSource string) {
	if account.IsZero() {
		return
	}

	r.mu.Lock()
	if r.cleared[account] {
		r.mu.Unlock()
		return
	}
	r.cleared[account] = true
	r.mu.Unlock()

	clearedCount, err := clearAccountAuthRequirementWithCount(ctx, account, config.AuthClearSourceCLIProof, r.logger)
	if err != nil {
		// Auth-scope normalization is best-effort maintenance. Successful direct
		// API commands must not surface stale auth proof cleanup failures to end
		// users.
		r.logger.Debug("clearing stale auth scopes after successful graph proof",
			"account", account.String(),
			"proof_source", proofSource,
			"error", err,
		)
		return
	}

	if clearedCount > 0 {
		r.logger.Info("cleared stale auth scopes after successful graph proof",
			"account", account.String(),
			"proof_source", proofSource,
			"state_dbs", clearedCount,
		)
	}
}

// attachAccountAuthProof makes every authenticated success of client a proof
// for account, the owner of the token client signs in with.
func attachAccountAuthProof(client *graph.Client, recorder *authProofRecorder, account driveid.CanonicalID, proofSource string) {
	if client == nil || recorder == nil || account.IsZero() {
		return
	}

	client.SetAuthenticatedSuccessHook(recorder.Hook(account, proofSource))
}

// attachDriveAuthProof is attachAccountAuthProof for both clients of a drive
// session, whose token account owns.
func attachDriveAuthProof(session interface {
	SetAuthenticatedSuccessHooks(func(context.Context))
}, recorder *authProofRecorder, account driveid.CanonicalID, proofSource string,
) {
	if session == nil || recorder == nil || account.IsZero() {
		return
	}

	session.SetAuthenticatedSuccessHooks(recorder.Hook(account, proofSource))
}

func inspectSavedLogin(
	_ context.Context,
	account string,
	driveIDs []driveid.CanonicalID,
	logger *slog.Logger,
) authstate.Reason {
	tokenID := canonicalIDForToken(account, driveIDs)
	if tokenID.IsZero() {
		accountCID, err := config.LoadAccountCanonicalIDByEmail(config.DefaultDataDir(), account)
		if err == nil {
			tokenID = accountCID
		}
		if tokenID.IsZero() {
			fallbackID, err := findTokenFallback(account, logger)
			if err != nil {
				logger.Warn("cannot check whether this account has a saved login",
					"account", account, "error", err)

				return authReasonSavedLoginUnreadable
			}
			tokenID = fallbackID
		}
	}

	tokenPath := config.DriveTokenPath(tokenID)
	if tokenPath == "" {
		return authReasonMissingLogin
	}
	exists, err := managedPathExists(tokenPath)
	if err != nil {
		// Not "missing login": a sign-in would write to the same unreadable
		// place, so the user is told to look at the directory instead.
		logger.Warn("cannot check whether this account has a saved login",
			"account", account, "path", tokenPath, "error", err)

		return authReasonSavedLoginUnreadable
	}
	if !exists {
		return authReasonMissingLogin
	}

	// Only check the file: the command loads the token for real later, and
	// that load is the one that logs "loaded saved token".
	err = graph.CheckSavedLogin(tokenPath)
	if err == nil {
		return ""
	}

	if errors.Is(err, graph.ErrNotLoggedIn) {
		return authReasonMissingLogin
	}

	return authReasonInvalidSavedLogin
}

func clearAccountAuthRequirementForSource(
	ctx context.Context,
	account driveid.CanonicalID,
	source config.AuthClearSource,
	logger *slog.Logger,
) error {
	_, err := clearAccountAuthRequirementWithCount(ctx, account, source, logger)
	return err
}

// clearAccountAuthRequirementWithCount ends the sign-in requirement of
// account, named by canonical ID, and reports whether one stood. It reads the
// catalog first so a proof that finds nothing to end writes nothing.
func clearAccountAuthRequirementWithCount(
	ctx context.Context,
	account driveid.CanonicalID,
	source config.AuthClearSource,
	logger *slog.Logger,
) (int, error) {
	if account.IsZero() {
		return 0, nil
	}

	required, err := config.LoadHasPersistedAccountAuthRequirement(config.DefaultDataDir(), account)
	if err != nil {
		return 0, fmt.Errorf("loading catalog: %w", err)
	}
	if !required {
		return 0, nil
	}

	if err := config.ClearAccountAuthRequirement(ctx, config.DefaultDataDir(), account); err != nil {
		return 0, fmt.Errorf("clearing auth requirement: %w", err)
	}

	logger.Info("cleared catalog auth requirement",
		"account", account.String(),
		"source", source,
	)
	return 1, nil
}

func authReasonText(reason authstate.Reason) string {
	return authstate.PresentationForReason(reason).Reason
}

func authAction(reason authstate.Reason) string {
	return authstate.PresentationForReason(reason).Action
}

func authRequirement(
	email string,
	displayName string,
	driveType string,
	stateDBs int,
	health accountAuthHealth,
) accountAuthRequirement {
	return accountAuthRequirement{
		Email:       email,
		DisplayName: displayName,
		DriveType:   driveType,
		Reason:      health.Reason,
		Action:      health.Action,
		StateDBs:    stateDBs,
	}
}

// mergeAuthRequirements merges the reports of one account from several
// sources into one entry. An account is its address and its drive type: a
// personal and a work or school account at one address are two entries.
func mergeAuthRequirements(groups ...[]accountAuthRequirement) []accountAuthRequirement {
	merged := make(map[string]accountAuthRequirement)

	for _, group := range groups {
		for i := range group {
			if group[i].Email == "" {
				continue
			}

			key := authRequirementKey(&group[i])
			if existing, ok := merged[key]; ok {
				if existing.DisplayName == "" {
					existing.DisplayName = group[i].DisplayName
				}
				if existing.DriveType == "" {
					existing.DriveType = group[i].DriveType
				}
				if existing.StateDBs == 0 {
					existing.StateDBs = group[i].StateDBs
				}
				if existing.Reason == "" {
					existing.Reason = group[i].Reason
				}
				if existing.Action == "" {
					existing.Action = group[i].Action
				}
				merged[key] = existing
				continue
			}

			merged[key] = group[i]
		}
	}

	result := make([]accountAuthRequirement, 0, len(merged))
	for _, item := range merged {
		result = append(result, item)
	}

	sortAccountAuthRequirements(result)

	return result
}

// authRequirementKey names the account an auth requirement is for. A work
// or school account's SharePoint report is the work or school account's.
func authRequirementKey(item *accountAuthRequirement) string {
	driveType := item.DriveType
	if driveType == driveid.DriveTypeSharePoint {
		driveType = driveid.DriveTypeBusiness
	}

	return item.Email + "|" + driveType
}

func sortAccountAuthRequirements(items []accountAuthRequirement) {
	sort.Slice(items, func(i, j int) bool {
		if items[i].Email != items[j].Email {
			return items[i].Email < items[j].Email
		}

		return items[i].DriveType < items[j].DriveType
	})
}

func printAccountAuthRequirementsText(w io.Writer, items []accountAuthRequirement) error {
	if len(items) == 0 {
		return nil
	}

	if err := writeln(w, "Authentication required:"); err != nil {
		return err
	}

	for _, acct := range items {
		nameLabel := acct.Email
		if acct.DisplayName != "" {
			nameLabel = fmt.Sprintf("%s (%s)", acct.DisplayName, acct.Email)
		}

		stateDBLabel := "no state databases"
		if acct.StateDBs > 0 {
			stateDBLabel = countOf(acct.StateDBs, "state database")
		}

		if err := writef(w, "  %s — %s, %s\n", nameLabel, acct.DriveType, stateDBLabel); err != nil {
			return err
		}
		if reasonText := authReasonText(acct.Reason); reasonText != "" {
			if err := writef(w, "    %s\n", reasonText); err != nil {
				return err
			}
		}
		if acct.Action != "" {
			if err := writef(w, "    %s\n", acct.Action); err != nil {
				return err
			}
		}
	}

	return nil
}

func authErrorMessage(err error) string {
	switch {
	case errors.Is(err, graph.ErrLoginCodeExpired):
		return "Error: the sign-in code expired before sign-in finished. Run 'onedrive-go login' again to get a new code."
	case errors.Is(err, graph.ErrLoginDeclined):
		return "Error: sign-in was canceled or declined. Run 'onedrive-go login' again."
	case errors.Is(err, graph.ErrLoginInteractionRequired):
		return "Authentication required: Microsoft requires you to sign in again (multi-factor authentication or a " +
			"sign-in policy asks for it). Run 'onedrive-go login'."
	case errors.Is(err, graph.ErrLoginAppNotAllowed):
		return "Error: this account or its organization has not allowed onedrive-go to access OneDrive. " +
			"Ask your administrator to approve the app, or sign in with a different account."
	case errors.Is(err, graph.ErrLoginConsumerNotEnabled):
		return "Error: this app registration accepts only work or school accounts, not personal Microsoft accounts. " +
			"Sign in with a work or school account, or use an app registration that allows personal accounts."
	case errors.Is(err, graph.ErrLoginExpired):
		return "Authentication required: Microsoft no longer accepts the saved login for this account " +
			"(it expired or was revoked). Run 'onedrive-go login' to sign in again."
	case errors.Is(err, graph.ErrNotLoggedIn):
		return "Authentication required: no saved login was found for this account. Run 'onedrive-go login'."
	case errors.Is(err, graph.ErrUnauthorized):
		return "Authentication required: OneDrive rejected the saved login for this account. Run 'onedrive-go login'."
	default:
		return ""
	}
}
