package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

func seedAuthScope(t *testing.T, cid driveid.CanonicalID) {
	t.Helper()

	store, err := syncengine.NewSyncStore(t.Context(), config.DriveStatePath(cid), testDriveLogger(t))
	require.NoError(t, err)
	defer store.Close(t.Context())

	require.NoError(t, store.UpsertScopeBlock(t.Context(), &syncengine.ScopeBlock{
		Key:          syncengine.SKAuthAccount(),
		IssueType:    syncengine.IssueUnauthorized,
		TimingSource: syncengine.ScopeTimingNone,
		BlockedAt:    time.Date(2026, 4, 2, 12, 0, 0, 0, time.UTC),
	}))

	accountCID := cid
	if cid.IsSharePoint() {
		accountCID = driveid.MustCanonicalID("business:" + cid.Email())
	}
	require.NoError(t, config.UpdateCatalog(func(catalog *config.Catalog) error {
		account := config.CatalogAccount{
			CanonicalID:           accountCID.String(),
			Email:                 accountCID.Email(),
			DriveType:             accountCID.DriveType(),
			AuthRequirementReason: authReasonSyncAuthRejected,
		}
		if existing, found := catalog.AccountByCanonicalID(accountCID); found {
			account = existing
			account.AuthRequirementReason = authReasonSyncAuthRejected
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
	require.True(t, hasPersistedAuthScope(t.Context(), "user@example.com", logger))

	require.NoError(t, clearAccountAuthScopes(t.Context(), "user@example.com", logger))
	assert.False(t, hasPersistedAuthScope(t.Context(), "user@example.com", logger))
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
	assert.False(t, hasPersistedAuthScope(t.Context(), cid.Email(), testDriveLogger(t)))
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
	assert.True(t, hasPersistedAuthScope(t.Context(), cid.Email(), testDriveLogger(t)))
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
	}

	require.NoError(t, runStatusCommand(cc, false))

	var decoded statusOutput
	require.NoError(t, json.Unmarshal(out.Bytes(), &decoded))
	require.Len(t, decoded.Accounts, 1)
	assert.Equal(t, authStateAuthenticationNeeded, decoded.Accounts[0].AuthState)
	assert.Equal(t, authReasonSyncAuthRejected, decoded.Accounts[0].AuthReason)
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
	}

	require.NoError(t, runStatusCommand(cc, false))
	assert.True(t, hasPersistedAuthScope(t.Context(), cid.Email(), testDriveLogger(t)))
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
	assert.Equal(t, "Run 'onedrive-go whoami' to re-check access, or 'onedrive-go login' to sign in again.", authAction(authReasonSyncAuthRejected))
	assert.Empty(t, authAction("unknown"))
}
