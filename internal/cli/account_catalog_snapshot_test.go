package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/tokenfile"
)

func writeAccessTokenFile(t *testing.T, cid driveid.CanonicalID, accessToken string) {
	t.Helper()

	tok := &oauth2.Token{
		AccessToken:  accessToken,
		RefreshToken: "refresh-" + accessToken,
		TokenType:    "Bearer",
		Expiry:       time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	require.NoError(t, tokenfile.Save(config.DriveTokenPath(cid), tok))
}

// Validates: R-3.7
func TestAccountCatalogSnapshot_LoadWithBestEffortIdentityRefresh_ProbesEachTokenOnce(t *testing.T) {
	setTestDriveHome(t)

	configPath := filepath.Join(t.TempDir(), "config.toml")
	oldCID := driveid.MustCanonicalID("personal:old@example.com")
	businessCID := driveid.MustCanonicalID("business:alice@contoso.com")

	require.NoError(t, config.AppendDriveSection(configPath, oldCID, "~/OneDrive"))
	require.NoError(t, config.SaveAccountProfile(oldCID, &config.AccountProfile{
		UserID:      "user-old",
		DisplayName: "Old User",
	}))
	require.NoError(t, config.SaveDriveIdentity(oldCID, &config.DriveIdentity{DriveID: "drive-old"}))
	require.NoError(t, config.SaveAccountProfile(businessCID, &config.AccountProfile{
		UserID:      "user-business",
		DisplayName: "Alice Smith",
	}))
	writeAccessTokenFile(t, oldCID, "token-old")
	writeAccessTokenFile(t, businessCID, "token-business")

	var mu sync.Mutex
	probes := make(map[string]int)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !assert.Equal(t, testGraphMePath, r.URL.Path) {
			http.Error(w, "unexpected path", http.StatusInternalServerError)
			return
		}

		authHeader := r.Header.Get("Authorization")
		mu.Lock()
		probes[authHeader]++
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch authHeader {
		case "Bearer token-old":
			writeTestResponse(t, w, `{
				"id":"user-old",
				"displayName":"Renamed User",
				"mail":"renamed@example.com"
			}`)
		case "Bearer token-business":
			writeTestResponse(t, w, `{
				"id":"user-business",
				"displayName":"Alice Smith",
				"mail":"alice@contoso.com"
			}`)
		default:
			assert.Failf(t, "unexpected authorization header", "authorization=%q", authHeader)
			http.Error(w, "unexpected authorization header", http.StatusUnauthorized)
		}
	}))
	defer srv.Close()

	cc := &CLIContext{
		Logger:       testDriveLogger(t),
		OutputWriter: &bytes.Buffer{},
		StatusWriter: &bytes.Buffer{},
		CfgPath:      configPath,
		GraphBaseURL: srv.URL,
	}

	snapshot, err := loadAccountCatalogSnapshotWithBestEffortIdentityRefresh(t.Context(), cc)
	require.NoError(t, err)

	assert.Len(t, snapshot.Catalog, 2)
	accountCatalogEntryByEmail(t, snapshot.Catalog, "renamed@example.com")
	accountCatalogEntryByEmail(t, snapshot.Catalog, "alice@contoso.com")

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 1, probes["Bearer token-old"])
	assert.Equal(t, 1, probes["Bearer token-business"])
}

// Validates: R-3.7
func TestAccountCatalogSnapshot_LoadWithBestEffortIdentityRefresh_ProbeFailureKeepsOfflineIdentity(t *testing.T) {
	setTestDriveHome(t)

	cid := driveid.MustCanonicalID("personal:offline@example.com")
	require.NoError(t, config.SaveAccountProfile(cid, &config.AccountProfile{
		UserID:      "user-offline",
		DisplayName: "Offline User",
	}))
	writeAccessTokenFile(t, cid, "token-offline")

	var meCalls atomic.Int32
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !assert.Equal(t, testGraphMePath, r.URL.Path) {
			http.Error(w, "unexpected path", http.StatusInternalServerError)
			return
		}
		meCalls.Add(1)
		cancel()
		w.WriteHeader(http.StatusInternalServerError)
		writeTestResponse(t, w, `{"error":{"code":"generalException","message":"probe failed"}}`)
	}))
	defer srv.Close()

	cc := &CLIContext{
		Logger:       testDriveLogger(t),
		OutputWriter: &bytes.Buffer{},
		StatusWriter: &bytes.Buffer{},
		CfgPath:      filepath.Join(t.TempDir(), "missing-config.toml"),
		GraphBaseURL: srv.URL,
	}

	snapshot, err := loadAccountCatalogSnapshotWithBestEffortIdentityRefresh(ctx, cc)
	require.NoError(t, err)

	entry := accountCatalogEntryByEmail(t, snapshot.Catalog, "offline@example.com")
	assert.Equal(t, "Offline User", entry.DisplayName)
	assert.Equal(t, cid, entry.RepresentativeTokenID)
	assert.GreaterOrEqual(t, meCalls.Load(), int32(1))
}

// Validates: R-3.1.5
func TestAccountCatalogSnapshot_Load_RejectsConfiguredDriveMissingCatalogEntry(t *testing.T) {
	setTestDriveHome(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cid := driveid.MustCanonicalID("personal:user@example.com")
	require.NoError(t, config.AppendDriveSection(cfgPath, cid, "~/OneDrive"))
	require.NoError(t, config.SaveAccountProfile(cid, &config.AccountProfile{
		UserID:      "user-123",
		DisplayName: "Test User",
	}))

	_, err := loadAccountCatalogSnapshot(t.Context(), &CLIContext{
		Logger:       testDriveLogger(t),
		OutputWriter: &bytes.Buffer{},
		StatusWriter: &bytes.Buffer{},
		CfgPath:      cfgPath,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "configured drive")
	assert.Contains(t, err.Error(), "has no catalog entry")
}

// Validates: R-3.1.5
func TestAccountCatalogSnapshot_Load_RejectsDriveOwnerMissingCatalogAccount(t *testing.T) {
	setTestDriveHome(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cid := driveid.MustCanonicalID("shared:user@example.com:drv123:item456")
	require.NoError(t, config.AppendDriveSection(cfgPath, cid, "~/Shared"))
	require.NoError(t, config.SaveDriveIdentity(cid, &config.DriveIdentity{
		AccountCanonicalID: "business:owner@example.com",
	}))

	_, err := loadAccountCatalogSnapshot(t.Context(), &CLIContext{
		Logger:       testDriveLogger(t),
		OutputWriter: &bytes.Buffer{},
		StatusWriter: &bytes.Buffer{},
		CfgPath:      cfgPath,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "owner")
	assert.Contains(t, err.Error(), "missing from the catalog")
}

// Validates: R-3.1.5
func TestAccountCatalogSnapshot_Load_RejectsPrimaryDriveOwnedByDifferentAccount(t *testing.T) {
	setTestDriveHome(t)

	accountCID := driveid.MustCanonicalID("personal:user@example.com")
	otherCID := driveid.MustCanonicalID("personal:other@example.com")
	require.NoError(t, config.SaveAccountProfile(accountCID, &config.AccountProfile{
		UserID:         "user-123",
		DisplayName:    "User",
		PrimaryDriveID: "drive-user",
	}))
	require.NoError(t, config.SaveAccountProfile(otherCID, &config.AccountProfile{
		UserID:      "user-other",
		DisplayName: "Other",
	}))
	require.NoError(t, config.UpdateCatalog(func(catalog *config.Catalog) error {
		account, found := catalog.AccountByCanonicalID(accountCID)
		require.True(t, found)
		account.PrimaryDriveCanonical = accountCID.String()
		catalog.UpsertAccount(&account)
		return nil
	}))
	require.NoError(t, config.SaveDriveIdentity(accountCID, &config.DriveIdentity{
		AccountCanonicalID: otherCID.String(),
		DriveID:            "drive-user",
	}))

	_, err := loadAccountCatalogSnapshot(t.Context(), &CLIContext{
		Logger:       testDriveLogger(t),
		OutputWriter: &bytes.Buffer{},
		StatusWriter: &bytes.Buffer{},
		CfgPath:      filepath.Join(t.TempDir(), "missing-config.toml"),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "primary drive")
	assert.Contains(t, err.Error(), "owned by")
}
