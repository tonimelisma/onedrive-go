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
