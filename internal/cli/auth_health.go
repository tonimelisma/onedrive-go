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
	authStateReady                = authstate.StateReady
	authStateAuthenticationNeeded = authstate.StateAuthenticationRequired
	authReasonMissingLogin        = authstate.ReasonMissingLogin
	authReasonInvalidSavedLogin   = authstate.ReasonInvalidSavedLogin
	authReasonSyncAuthRejected    = authstate.ReasonSyncAuthRejected
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

type accountAuthChecker interface {
	CheckAccountAuth(ctx context.Context, account string, driveIDs []driveid.CanonicalID) accountAuthHealth
}

type authProofRecorder struct {
	logger  *slog.Logger
	mu      sync.Mutex // guards cleared so one successful proof clears scopes once per account
	cleared map[string]bool
}

func newAuthProofRecorder(logger *slog.Logger) *authProofRecorder {
	return &authProofRecorder{
		logger:  logger,
		cleared: make(map[string]bool),
	}
}

func (r *authProofRecorder) Hook(email, proofSource string) func(context.Context) {
	return func(ctx context.Context) {
		r.recordSuccess(ctx, email, proofSource)
	}
}

func (r *authProofRecorder) recordSuccess(ctx context.Context, email, proofSource string) {
	if email == "" {
		return
	}

	r.mu.Lock()
	if r.cleared[email] {
		r.mu.Unlock()
		return
	}
	r.cleared[email] = true
	r.mu.Unlock()

	stored, loadErr := config.LoadCatalog()
	if loadErr != nil {
		r.logger.Debug("loading catalog for auth proof cleanup", "account", email, "error", loadErr)
		return
	}

	clearedCount, err := clearAccountAuthRequirementWithCount(ctx, stored, email, r.logger)
	if err != nil {
		// Auth-scope repair is best-effort maintenance. Successful direct API
		// commands must not surface sync-store repair failures to end users just
		// because stale auth proof cleanup could not run.
		r.logger.Debug("clearing stale auth scopes after successful graph proof",
			"account", email,
			"proof_source", proofSource,
			"error", err,
		)
		return
	}

	if clearedCount > 0 {
		r.logger.Info("cleared stale auth scopes after successful graph proof",
			"account", email,
			"proof_source", proofSource,
			"state_dbs", clearedCount,
		)
	}
}

func attachAccountAuthProof(client *graph.Client, recorder *authProofRecorder, email, proofSource string) {
	if client == nil || recorder == nil || email == "" {
		return
	}

	client.SetAuthenticatedSuccessHook(recorder.Hook(email, proofSource))
}

func attachDriveAuthProof(session interface {
	ResolvedDriveEmail() string
	SetAuthenticatedSuccessHooks(func(context.Context))
}, recorder *authProofRecorder, proofSource string,
) {
	if session == nil || recorder == nil {
		return
	}

	email := session.ResolvedDriveEmail()
	if email == "" {
		return
	}

	session.SetAuthenticatedSuccessHooks(recorder.Hook(email, proofSource))
}

func inspectAccountAuth(
	ctx context.Context,
	account string,
	driveIDs []driveid.CanonicalID,
	logger *slog.Logger,
) accountAuthHealth {
	savedLoginReason := inspectSavedLogin(ctx, account, driveIDs, logger)
	if savedLoginReason != "" {
		return authstate.RequiredHealth(savedLoginReason)
	}

	if hasPersistedAccountAuthRequirement(ctx, account, logger) {
		return authstate.RequiredHealth(authReasonSyncAuthRejected)
	}

	return authstate.ReadyHealth()
}

func inspectSavedLogin(
	ctx context.Context,
	account string,
	driveIDs []driveid.CanonicalID,
	logger *slog.Logger,
) authstate.Reason {
	tokenID := canonicalIDForToken(account, driveIDs)
	if tokenID.IsZero() {
		stored, err := config.LoadCatalog()
		if err == nil {
			tokenID = catalogAccountTokenCID(stored, account)
		}
		if tokenID.IsZero() {
			tokenID = findTokenFallback(account, logger)
		}
	}

	tokenPath := config.DriveTokenPath(tokenID)
	if tokenPath == "" || !managedPathExists(tokenPath) {
		return authReasonMissingLogin
	}

	_, err := graph.TokenSourceFromPath(ctx, tokenPath, logger)
	if err == nil {
		return ""
	}

	if errors.Is(err, graph.ErrNotLoggedIn) {
		return authReasonMissingLogin
	}

	return authReasonInvalidSavedLogin
}

func hasPersistedAccountAuthRequirement(ctx context.Context, account string, logger *slog.Logger) bool {
	_ = ctx
	stored, err := config.LoadCatalog()
	if err != nil {
		logger.Debug("loading catalog for auth projection", "account", account, "error", err)
		return false
	}

	accountEntry, found := stored.AccountByEmail(account)
	if !found {
		return false
	}

	return accountEntry.AuthRequirementReason == authReasonSyncAuthRejected
}

func clearAccountAuthRequirement(ctx context.Context, email string, logger *slog.Logger) error {
	_, err := clearAccountAuthRequirementWithCount(ctx, nil, email, logger)
	return err
}

func clearAccountAuthRequirementWithCatalog(ctx context.Context, stored *config.Catalog, email string, logger *slog.Logger) error {
	_, err := clearAccountAuthRequirementWithCount(ctx, stored, email, logger)
	return err
}

func clearAccountAuthRequirementWithCount(ctx context.Context, stored *config.Catalog, email string, logger *slog.Logger) (int, error) {
	if email == "" {
		return 0, nil
	}

	_ = ctx
	if stored == nil {
		var err error
		stored, err = config.LoadCatalog()
		if err != nil {
			return 0, fmt.Errorf("loading catalog: %w", err)
		}
	}

	accountEntry, found := stored.AccountByEmail(email)
	if !found || accountEntry.AuthRequirementReason == "" {
		return 0, nil
	}

	accountEntry.AuthRequirementReason = ""
	stored.UpsertAccount(&accountEntry)
	if err := config.SaveCatalog(stored); err != nil {
		return 0, fmt.Errorf("writing catalog: %w", err)
	}

	logger.Info("cleared catalog auth requirement after successful proof", "account", email)
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

func mergeAuthRequirements(groups ...[]accountAuthRequirement) []accountAuthRequirement {
	merged := make(map[string]accountAuthRequirement)

	for _, group := range groups {
		for i := range group {
			if group[i].Email == "" {
				continue
			}

			if existing, ok := merged[group[i].Email]; ok {
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
				merged[group[i].Email] = existing
				continue
			}

			merged[group[i].Email] = group[i]
		}
	}

	result := make([]accountAuthRequirement, 0, len(merged))
	for _, item := range merged {
		result = append(result, item)
	}

	sortAccountAuthRequirements(result)

	return result
}

func sortAccountAuthRequirements(items []accountAuthRequirement) {
	sort.Slice(items, func(i, j int) bool {
		return items[i].Email < items[j].Email
	})
}

func printAccountAuthRequirementsText(w io.Writer, header string, items []accountAuthRequirement) error {
	if len(items) == 0 {
		return nil
	}

	if err := writeln(w, header); err != nil {
		return err
	}

	for _, acct := range items {
		nameLabel := acct.Email
		if acct.DisplayName != "" {
			nameLabel = fmt.Sprintf("%s (%s)", acct.DisplayName, acct.Email)
		}

		stateDBLabel := "no state databases"
		switch acct.StateDBs {
		case 1:
			stateDBLabel = "1 state database"
		default:
			if acct.StateDBs > 1 {
				stateDBLabel = fmt.Sprintf("%d state databases", acct.StateDBs)
			}
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
	case errors.Is(err, graph.ErrNotLoggedIn):
		return "Authentication required: no saved login was found for this account. Run 'onedrive-go login'."
	case errors.Is(err, graph.ErrUnauthorized):
		return "Authentication required: OneDrive rejected the saved login for this account. Run 'onedrive-go login'."
	default:
		return ""
	}
}
