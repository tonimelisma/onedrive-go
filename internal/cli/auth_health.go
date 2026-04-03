package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"sync"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

const (
	authStateReady                = "ready"
	authStateAuthenticationNeeded = "authentication_required"
	authReasonMissingLogin        = "missing_login"
	authReasonInvalidSavedLogin   = "invalid_saved_login"
	authReasonSyncAuthRejected    = "sync_auth_rejected"
)

type accountAuthHealth struct {
	State  string
	Reason string
	Action string
}

type accountAuthRequirement struct {
	Email       string `json:"email"`
	DisplayName string `json:"display_name,omitempty"`
	DriveType   string `json:"drive_type,omitempty"`
	Reason      string `json:"reason"`
	Action      string `json:"action,omitempty"`
	StateDBs    int    `json:"state_dbs,omitempty"`
}

type accountAuthChecker interface {
	CheckAccountAuth(ctx context.Context, account string, driveIDs []driveid.CanonicalID) accountAuthHealth
}

type liveAccountAuthChecker struct {
	logger *slog.Logger
}

func (c *liveAccountAuthChecker) CheckAccountAuth(
	ctx context.Context,
	account string,
	driveIDs []driveid.CanonicalID,
) accountAuthHealth {
	return inspectAccountAuth(ctx, account, driveIDs, c.logger)
}

type authProofRecorder struct {
	logger  *slog.Logger
	mu      sync.Mutex
	cleared map[string]bool
}

func newAuthProofRecorder(logger *slog.Logger) *authProofRecorder {
	return &authProofRecorder{
		logger:  logger,
		cleared: make(map[string]bool),
	}
}

func (r *authProofRecorder) Hook(email string) func(context.Context) {
	return func(ctx context.Context) {
		r.recordSuccess(ctx, email)
	}
}

func (r *authProofRecorder) recordSuccess(ctx context.Context, email string) {
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

	if err := clearAccountAuthScopes(ctx, email, r.logger); err != nil {
		r.logger.Warn("clearing stale auth scopes after successful graph proof",
			"account", email,
			"error", err,
		)
	}
}

func attachAccountAuthProof(client *graph.Client, recorder *authProofRecorder, email string) {
	if client == nil || recorder == nil || email == "" {
		return
	}

	client.SetAuthenticatedSuccessHook(recorder.Hook(email))
}

func attachDriveAuthProof(session interface {
	ResolvedDriveEmail() string
	SetAuthenticatedSuccessHooks(func(context.Context))
}, recorder *authProofRecorder,
) {
	if session == nil || recorder == nil {
		return
	}

	email := session.ResolvedDriveEmail()
	if email == "" {
		return
	}

	session.SetAuthenticatedSuccessHooks(recorder.Hook(email))
}

func inspectAccountAuth(
	ctx context.Context,
	account string,
	driveIDs []driveid.CanonicalID,
	logger *slog.Logger,
) accountAuthHealth {
	savedLoginReason := inspectSavedLogin(ctx, account, driveIDs, logger)
	if savedLoginReason != "" {
		return accountAuthHealth{
			State:  authStateAuthenticationNeeded,
			Reason: savedLoginReason,
			Action: authAction(savedLoginReason),
		}
	}

	if hasPersistedAuthScope(ctx, account, logger) {
		return accountAuthHealth{
			State:  authStateAuthenticationNeeded,
			Reason: authReasonSyncAuthRejected,
			Action: authAction(authReasonSyncAuthRejected),
		}
	}

	return accountAuthHealth{State: authStateReady}
}

func inspectSavedLogin(
	ctx context.Context,
	account string,
	driveIDs []driveid.CanonicalID,
	logger *slog.Logger,
) string {
	tokenID := canonicalIDForToken(account, driveIDs)
	if tokenID.IsZero() {
		tokenID = findTokenFallback(account, logger)
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

func hasPersistedAuthScope(ctx context.Context, account string, logger *slog.Logger) bool {
	for _, statePath := range config.DiscoverStateDBsForEmail(account, logger) {
		store, err := syncstore.NewSyncStore(ctx, statePath, logger)
		if err != nil {
			logger.Debug("opening state DB for auth projection",
				"account", account,
				"path", statePath,
				"error", err,
			)
			continue
		}

		blocks, listErr := store.ListScopeBlocks(ctx)
		closeErr := store.Close(ctx)
		if listErr != nil {
			logger.Debug("listing scope blocks for auth projection",
				"account", account,
				"path", statePath,
				"error", listErr,
			)
			if closeErr != nil {
				logger.Debug("closing state DB after auth projection failure",
					"account", account,
					"path", statePath,
					"error", closeErr,
				)
			}
			continue
		}

		if closeErr != nil {
			logger.Debug("closing state DB after auth projection",
				"account", account,
				"path", statePath,
				"error", closeErr,
			)
		}

		for i := range blocks {
			if blocks[i].Key == synctypes.SKAuthAccount() {
				return true
			}
		}
	}

	return false
}

func clearAccountAuthScopes(ctx context.Context, email string, logger *slog.Logger) error {
	if email == "" {
		return nil
	}

	var errs []error

	for _, statePath := range config.DiscoverStateDBsForEmail(email, logger) {
		store, err := syncstore.NewSyncStore(ctx, statePath, logger)
		if err != nil {
			errs = append(errs, fmt.Errorf("open sync store %s: %w", statePath, err))
			continue
		}

		if err := store.DeleteScopeBlock(ctx, synctypes.SKAuthAccount()); err != nil {
			errs = append(errs, fmt.Errorf("delete auth scope from %s: %w", statePath, err))
		}

		if err := store.Close(ctx); err != nil {
			errs = append(errs, fmt.Errorf("close sync store %s: %w", statePath, err))
		}
	}

	return errors.Join(errs...)
}

func authReasonText(reason string) string {
	switch reason {
	case authReasonMissingLogin:
		return "No saved login was found for this account."
	case authReasonInvalidSavedLogin:
		return "The saved login for this account is invalid or unreadable."
	case authReasonSyncAuthRejected:
		return "The last sync attempt for this account was rejected by OneDrive."
	default:
		return ""
	}
}

func authAction(reason string) string {
	switch reason {
	case authReasonSyncAuthRejected:
		return "Run 'onedrive-go whoami' to re-check access, or 'onedrive-go login' to sign in again."
	case authReasonMissingLogin, authReasonInvalidSavedLogin:
		return "Run 'onedrive-go login' to sign in."
	default:
		return ""
	}
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
