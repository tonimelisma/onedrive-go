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

const (
	snapshotTestDisplayNameAliceSmith = "Alice Smith"
	snapshotTestUserID123             = "user-123"
	snapshotTestPrimaryDriveUser      = "drive-user"
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
func TestAccountViewSnapshot_LoadWithBestEffortIdentityRefresh_ProbesEachTokenOnce(t *testing.T) {
	setTestDriveHome(t)

	configPath := filepath.Join(t.TempDir(), "config.toml")
	oldCID := driveid.MustCanonicalID("personal:old@example.com")
	businessCID := driveid.MustCanonicalID("business:alice@contoso.com")

	require.NoError(t, config.AppendDriveSection(configPath, oldCID, "~/OneDrive"))
	seedCatalogAccount(t, oldCID, func(account *config.CatalogAccount) {
		account.UserID = "user-old"
		account.DisplayName = "Old User"
	})
	seedCatalogDrive(t, oldCID, func(drive *config.CatalogDrive) {
		drive.RemoteDriveID = "drive-old"
	})
	seedCatalogAccount(t, businessCID, func(account *config.CatalogAccount) {
		account.UserID = "user-business"
		account.DisplayName = snapshotTestDisplayNameAliceSmith
	})
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

	snapshot, err := loadAccountViewSnapshotWithBestEffortIdentityRefresh(t.Context(), cc)
	require.NoError(t, err)

	assert.Len(t, snapshot.Accounts, 2)
	_, found := accountViewByEmail(snapshot.Accounts, "renamed@example.com")
	require.True(t, found)
	_, found = accountViewByEmail(snapshot.Accounts, "alice@contoso.com")
	require.True(t, found)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 1, probes["Bearer token-old"])
	assert.Equal(t, 1, probes["Bearer token-business"])
}

// Validates: R-3.7
func TestAccountViewSnapshot_LoadWithBestEffortIdentityRefresh_ProbeFailureKeepsOfflineIdentity(t *testing.T) {
	setTestDriveHome(t)

	cid := driveid.MustCanonicalID("personal:offline@example.com")
	seedCatalogAccount(t, cid, func(account *config.CatalogAccount) {
		account.UserID = "user-offline"
		account.DisplayName = "Offline User"
	})
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

	snapshot, err := loadAccountViewSnapshotWithBestEffortIdentityRefresh(ctx, cc)
	require.NoError(t, err)

	entry, found := accountViewByEmail(snapshot.Accounts, "offline@example.com")
	require.True(t, found)
	assert.Equal(t, "Offline User", entry.DisplayName)
	assert.Equal(t, cid, entry.RepresentativeTokenID)
	assert.GreaterOrEqual(t, meCalls.Load(), int32(1))
}

// Validates: R-3.1.5
func TestAccountViewSnapshot_Load_RejectsConfiguredDriveMissingCatalogEntry(t *testing.T) {
	setTestDriveHome(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cid := driveid.MustCanonicalID("personal:user@example.com")
	require.NoError(t, config.AppendDriveSection(cfgPath, cid, "~/OneDrive"))
	seedCatalogAccount(t, cid, func(account *config.CatalogAccount) {
		account.UserID = snapshotTestUserID123
		account.DisplayName = "Test User"
	})

	_, err := loadAccountViewSnapshot(t.Context(), &CLIContext{
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
func TestAccountViewSnapshot_Load_RejectsDriveOwnerMissingCatalogAccount(t *testing.T) {
	setTestDriveHome(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cid := driveid.MustCanonicalID("shared:user@example.com:drv123:item456")
	require.NoError(t, config.AppendDriveSection(cfgPath, cid, "~/Shared"))
	seedCatalogDrive(t, cid, func(drive *config.CatalogDrive) {
		drive.OwnerAccountCanonical = "business:owner@example.com"
	})

	_, err := loadAccountViewSnapshot(t.Context(), &CLIContext{
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
func TestAccountViewSnapshot_Load_RejectsPrimaryDriveOwnedByDifferentAccount(t *testing.T) {
	setTestDriveHome(t)

	accountCID := driveid.MustCanonicalID("personal:user@example.com")
	otherCID := driveid.MustCanonicalID("personal:other@example.com")
	seedCatalogAccount(t, accountCID, func(account *config.CatalogAccount) {
		account.UserID = snapshotTestUserID123
		account.DisplayName = "User"
		account.PrimaryDriveID = snapshotTestPrimaryDriveUser
	})
	seedCatalogAccount(t, otherCID, func(account *config.CatalogAccount) {
		account.UserID = "user-other"
		account.DisplayName = "Other"
	})
	require.NoError(t, config.UpdateCatalog(func(catalog *config.Catalog) error {
		account, found := catalog.AccountByCanonicalID(accountCID)
		require.True(t, found)
		account.PrimaryDriveCanonical = accountCID.String()
		catalog.UpsertAccount(&account)
		return nil
	}))
	seedCatalogDrive(t, accountCID, func(drive *config.CatalogDrive) {
		drive.OwnerAccountCanonical = otherCID.String()
		drive.RemoteDriveID = snapshotTestPrimaryDriveUser
	})

	_, err := loadAccountViewSnapshot(t.Context(), &CLIContext{
		Logger:       testDriveLogger(t),
		OutputWriter: &bytes.Buffer{},
		StatusWriter: &bytes.Buffer{},
		CfgPath:      filepath.Join(t.TempDir(), "missing-config.toml"),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "primary drive")
	assert.Contains(t, err.Error(), "owned by")
}
