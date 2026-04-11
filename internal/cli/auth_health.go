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
	"github.com/tonimelisma/onedrive-go/internal/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
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

	clearedCount, err := clearAccountAuthScopesWithCount(ctx, email, r.logger)
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

	if hasPersistedAuthScope(ctx, account, logger) {
		return authstate.RequiredHealth(authReasonSyncAuthRejected)
	}

	return authstate.ReadyHealth()
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
		hasBlock, err := syncstore.HasScopeBlockAtPath(ctx, statePath, synctypes.SKAuthAccount(), logger)
		if err != nil {
			logger.Debug("reading auth scope block for auth projection",
				"account", account,
				"path", statePath,
				"error", err,
			)
			continue
		}

		if hasBlock {
			return true
		}
	}

	return false
}

func clearAccountAuthScopes(ctx context.Context, email string, logger *slog.Logger) error {
	_, err := clearAccountAuthScopesWithCount(ctx, email, logger)
	return err
}

func clearAccountAuthScopesWithCount(ctx context.Context, email string, logger *slog.Logger) (int, error) {
	if email == "" {
		return 0, nil
	}

	var errs []error
	clearedCount := 0

	for _, statePath := range config.DiscoverStateDBsForEmail(email, logger) {
		store, err := syncstore.NewSyncStore(ctx, statePath, logger)
		if err != nil {
			errs = append(errs, fmt.Errorf("open sync store %s: %w", statePath, err))
			continue
		}

		blocks, listErr := store.ListScopeBlocks(ctx)
		if listErr != nil {
			errs = append(errs, fmt.Errorf("list scope blocks %s: %w", statePath, listErr))
		} else if scopeBlocksContainAuth(blocks) {
			if err := store.DeleteScopeBlock(ctx, synctypes.SKAuthAccount()); err != nil {
				errs = append(errs, fmt.Errorf("delete auth scope from %s: %w", statePath, err))
			} else {
				clearedCount++
			}
		}

		if err := store.Close(ctx); err != nil {
			errs = append(errs, fmt.Errorf("close sync store %s: %w", statePath, err))
		}
	}

	return clearedCount, errors.Join(errs...)
}

func scopeBlocksContainAuth(blocks []*synctypes.ScopeBlock) bool {
	for _, block := range blocks {
		if block.Key == synctypes.SKAuthAccount() {
			return true
		}
	}

	return false
}

func authReasonText(reason string) string {
	return authstate.PresentationForReason(reason).Reason
}

func authAction(reason string) string {
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
	return authstate.ErrorMessage(err)
}
