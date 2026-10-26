package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/authstate"
	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

func seedAuthScope(t *testing.T, cid driveid.CanonicalID) {
	t.Helper()

	seedCatalogDrive(t, cid, func(drive *config.CatalogDrive) {
		if drive.RemoteDriveID == "" {
			drive.RemoteDriveID = "drive-auth"
		}
	})
	store, err := syncengine.NewSyncStore(t.Context(), config.DriveStatePath(cid), testDriveLogger(t))
	require.NoError(t, err)
	require.NoError(t, store.Close(t.Context()))

	accountCID := cid
	if cid.IsSharePoint() {
		accountCID = driveid.MustCanonicalID("business:" + cid.Email())
	}
	require.NoError(t, config.UpdateCatalog(func(catalog *config.Catalog) error {
		account := config.CatalogAccount{
			CanonicalID:           accountCID.String(),
			Email:                 accountCID.Email(),
			DriveType:             accountCID.DriveType(),
			AuthRequirementReason: authstate.ReasonSyncAuthRejected,
		}
		if existing, found := catalog.AccountByCanonicalID(accountCID); found {
			account = existing
			account.AuthRequirementReason = authstate.ReasonSyncAuthRejected
		}
		catalog.UpsertAccount(&account)
		return nil
	}))
}

// Validates: R-2.10.47
func TestClearAccountAuthScopes_ClearsPersistedAuthScope(t *testing.T) {
	setTestDriveHome(t)

	cids := []driveid.CanonicalID{
		driveid.MustCanonicalID("personal:user@example.com"),
		driveid.MustCanonicalID("business:user@example.com"),
	}
	for _, cid := range cids {
		seedAuthScope(t, cid)
	}

	logger := testDriveLogger(t)
	require.True(t, hasPersistedAccountAuthRequirement("user@example.com", logger))

	require.NoError(t, clearAccountAuthRequirement("user@example.com", logger))
	assert.False(t, hasPersistedAccountAuthRequirement("user@example.com", logger))
}

// Validates: R-2.10.47
func TestAttachAccountAuthProof_ClearsOnAuthenticatedSuccess(t *testing.T) {
	setTestDriveHome(t)

	cid := driveid.MustCanonicalID("personal:user@example.com")
	seedAuthScope(t, cid)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/me", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, err := w.Write([]byte(`{
			"id": "user-123",
			"displayName": "Test User",
			"mail": "user@example.com",
			"userPrincipalName": "user@example.com"
		}`))
		assert.NoError(t, err)
	}))
	defer srv.Close()

	client := newTestGraphClient(t, srv.URL)
	attachAccountAuthProof(client, newAuthProofRecorder(testDriveLogger(t)), cid.Email(), "test")

	_, err := client.Me(t.Context())
	require.NoError(t, err)
	assert.False(t, hasPersistedAccountAuthRequirement(cid.Email(), testDriveLogger(t)))
}

// Validates: R-2.10.47
func TestAttachAccountAuthProof_DoesNotClearOnUnauthorized(t *testing.T) {
	setTestDriveHome(t)

	cid := driveid.MustCanonicalID("personal:user@example.com")
	seedAuthScope(t, cid)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, err := w.Write([]byte(`{"error":{"code":"InvalidAuthenticationToken","message":"unauthorized"}}`))
		assert.NoError(t, err)
	}))
	defer srv.Close()

	client := newTestGraphClient(t, srv.URL)
	attachAccountAuthProof(client, newAuthProofRecorder(testDriveLogger(t)), cid.Email(), "test")

	_, err := client.Me(t.Context())
	require.ErrorIs(t, err, graph.ErrUnauthorized)
	assert.True(t, hasPersistedAccountAuthRequirement(cid.Email(), testDriveLogger(t)))
}

// Validates: R-2.10.45, R-2.10.47
func TestStatusCommand_JSONSurfacesSyncAuthRejectedOffline(t *testing.T) {
	setTestDriveHome(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cid := driveid.MustCanonicalID("personal:user@example.com")
	require.NoError(t, config.AppendDriveSection(cfgPath, cid, "~/OneDrive"))
	writeTestTokenFile(t, config.DefaultDataDir(), "token_personal_user@example.com.json")
	seedAuthScope(t, cid)

	var out bytes.Buffer
	cc := &CLIContext{
		Logger:       testDriveLogger(t),
		OutputWriter: &out,
		CfgPath:      cfgPath,
		Flags:        CLIFlags{JSON: true},
		seams:        cliSeams{statusLiveOverlay: noStatusLiveOverlay},
	}

	require.NoError(t, runStatusCommand(t.Context(), cc, false))

	var decoded statusOutput
	require.NoError(t, json.Unmarshal(out.Bytes(), &decoded))
	require.Len(t, decoded.Accounts, 1)
	require.NotNil(t, decoded.Accounts[0].SignInRequired)
	assert.Contains(t, decoded.Accounts[0].SignInRequired.Reason, "rejected by OneDrive")
	assert.Equal(t, 1, decoded.Summary.AccountsRequiringAuth)
}

// Validates: R-2.10.47
func TestStatusCommand_DoesNotClearPersistedAuthScope(t *testing.T) {
	setTestDriveHome(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cid := driveid.MustCanonicalID("personal:user@example.com")
	require.NoError(t, config.AppendDriveSection(cfgPath, cid, "~/OneDrive"))
	writeTestTokenFile(t, config.DefaultDataDir(), "token_personal_user@example.com.json")
	seedAuthScope(t, cid)

	var out bytes.Buffer
	cc := &CLIContext{
		Logger:       testDriveLogger(t),
		OutputWriter: &out,
		StatusWriter: &out,
		CfgPath:      cfgPath,
		seams:        cliSeams{statusLiveOverlay: noStatusLiveOverlay},
	}

	require.NoError(t, runStatusCommand(t.Context(), cc, false))
	assert.True(t, hasPersistedAccountAuthRequirement(cid.Email(), testDriveLogger(t)))
}

// Validates: R-2.10.45
func TestAuthErrorMessage_UsesUnifiedAuthenticationRequiredWording(t *testing.T) {
	notLoggedIn := authErrorMessage(graph.ErrNotLoggedIn)
	assert.Contains(t, notLoggedIn, "Authentication required:")
	assert.Contains(t, notLoggedIn, "onedrive-go login")

	unauthorized := authErrorMessage(graph.ErrUnauthorized)
	assert.Contains(t, unauthorized, "Authentication required:")
	assert.Contains(t, unauthorized, "OneDrive rejected the saved login")
}

// Validates: R-6.6.34
func TestAuthErrorMessage_AADOutcomesArePlain(t *testing.T) {
	t.Parallel()

	mfa := authErrorMessage(fmt.Errorf("refreshing access token: %w: %w", graph.ErrLoginInteractionRequired, graph.ErrUnauthorized))
	assert.Equal(t, "Authentication required: Microsoft requires you to sign in again (multi-factor authentication or a "+
		"sign-in policy asks for it). Run 'onedrive-go login'.", mfa)
	assert.Equal(t, exitSignInRequired, commandExitCode(
		fmt.Errorf("x: %w: %w", graph.ErrLoginInteractionRequired, graph.ErrUnauthorized), false))

	assert.Contains(t, authErrorMessage(graph.ErrLoginAppNotAllowed), "has not allowed onedrive-go")
	assert.Contains(t, authErrorMessage(graph.ErrLoginConsumerNotEnabled), "accepts only work or school accounts")
	assert.Empty(t, authErrorMessage(graph.ErrAuthTransient), "a transient auth fault is not a sign-in message")

	// What the token bridge returns for these outcomes at refresh: status must
	// report the saved login as no longer accepted, not "rejected with a
	// still-valid login".
	for _, outcome := range []error{graph.ErrLoginInteractionRequired, graph.ErrLoginAppNotAllowed, graph.ErrLoginConsumerNotEnabled} {
		refreshErr := fmt.Errorf("refreshing access token: %w: %w: %w", outcome, graph.ErrLoginExpired, graph.ErrUnauthorized)
		assert.Equal(t, authReasonSavedLoginExpired, unauthorizedAuthReason(refreshErr), outcome.Error())
	}
}

func TestMergeAuthRequirements_PrefersExistingFieldsAndSorts(t *testing.T) {
	t.Parallel()

	merged := mergeAuthRequirements(
		[]accountAuthRequirement{
			authRequirement("b@example.com", "", "", 0, accountAuthHealth{
				Reason: authReasonMissingLogin,
				Action: authAction(authReasonMissingLogin),
			}),
			authRequirement("a@example.com", "Alpha", "personal", 1, accountAuthHealth{
				Reason: authReasonSyncAuthRejected,
				Action: authAction(authReasonSyncAuthRejected),
			}),
		},
		[]accountAuthRequirement{
			authRequirement("b@example.com", "Bravo", "business", 2, accountAuthHealth{
				Reason: authReasonInvalidSavedLogin,
				Action: authAction(authReasonInvalidSavedLogin),
			}),
			{},
		},
	)

	require.Len(t, merged, 2)
	assert.Equal(t, "a@example.com", merged[0].Email)
	assert.Equal(t, "b@example.com", merged[1].Email)
	assert.Equal(t, "Bravo", merged[1].DisplayName)
	assert.Equal(t, "business", merged[1].DriveType)
	assert.Equal(t, 2, merged[1].StateDBs)
	assert.Equal(t, authReasonMissingLogin, merged[1].Reason)
	assert.Equal(t, authAction(authReasonMissingLogin), merged[1].Action)
}

func TestAuthReasonTextAndAction_ReturnExpectedStrings(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "No saved login was found for this account.", authReasonText(authReasonMissingLogin))
	assert.Equal(t, "The saved login for this account is invalid or unreadable.", authReasonText(authReasonInvalidSavedLogin))
	assert.Equal(t, "The last sync attempt for this account was rejected by OneDrive.", authReasonText(authReasonSyncAuthRejected))
	assert.Empty(t, authReasonText("unknown"))

	assert.Equal(t, "Run 'onedrive-go login' to sign in.", authAction(authReasonMissingLogin))
	assert.Equal(t, "Run 'onedrive-go login' to sign in.", authAction(authReasonInvalidSavedLogin))
	assert.Equal(t, "Run 'onedrive-go status' to re-check access, or 'onedrive-go login' to sign in again.", authAction(authReasonSyncAuthRejected))
	assert.Empty(t, authAction("unknown"))
}

// Checking a saved login is not loading it: the health check behind status
// and drive list only reads the file, so a command that then loads the token
// for real logs "loaded saved token" once per run, not twice.
func TestInspectSavedLogin_DoesNotLogATokenLoad(t *testing.T) {
	setTestDriveHome(t)

	cid := driveid.MustCanonicalID("personal:user@example.com")
	writeTestTokenFile(t, config.DefaultDataDir(), "token_personal_user@example.com.json")

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	assert.Empty(t, inspectSavedLogin(t.Context(), cid.Email(), []driveid.CanonicalID{cid}, logger))
	assert.NotContains(t, logs.String(), "loaded saved token")
}
