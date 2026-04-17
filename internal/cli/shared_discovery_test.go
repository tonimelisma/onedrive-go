package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

const testSharedDiscoveryRemoteFolderPath = "/drives/00000b!remote123/items/remote-folder-1"

// Validates: R-3.6.2, R-3.6.4, R-3.6.6, R-3.6.7
func TestSharedDiscovery_IgnoresNonActionableHits(t *testing.T) {
	setTestDriveHome(t)
	writeTestTokenFile(t, config.DefaultDataDir(), "token_personal_user@example.com.json")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case testGraphMePath:
			writeTestResponse(t, w, `{
				"id":"user-123",
				"displayName":"User Example",
				"mail":"user@example.com"
			}`)
		case testDriveSearchAllPath:
			writeTestResponse(t, w, `{
				"value": [
					{
						"id":"local-ignored",
						"name":"Ignored Item",
						"folder":{}
					},
					{
						"id":"local-kept",
						"name":"Kept Folder",
						"folder":{"childCount":1},
						"remoteItem":{
							"id":"remote-folder-1",
							"parentReference":{"driveId":"b!remote123"}
						}
					}
				]
			}`)
		case testSharedDiscoveryRemoteFolderPath:
			writeTestResponse(t, w, `{
				"id":"remote-folder-1",
				"name":"Kept Folder",
				"folder":{"childCount":1},
				"parentReference":{"driveId":"00000b!remote123"},
				"shared":{"owner":{"user":{"email":"alice@example.com","displayName":"Alice"}}}
			}`)
		default:
			assert.Failf(t, "unexpected path", "path=%s", r.URL.Path)
		}
	}))
	defer srv.Close()

	cc := &CLIContext{
		Logger:       testDriveLogger(t),
		OutputWriter: &bytes.Buffer{},
		StatusWriter: &bytes.Buffer{},
		CfgPath:      filepath.Join(t.TempDir(), "config.toml"),
		GraphBaseURL: srv.URL,
	}

	result := discoverSharedTargets(t.Context(), cc, []accountView{{
		Email:                 "user@example.com",
		DisplayName:           "User Example",
		DriveType:             driveid.DriveTypePersonal,
		RepresentativeTokenID: driveid.MustCanonicalID("personal:user@example.com"),
		AuthHealth:            accountAuthHealth{State: authStateReady},
	}})

	require.Len(t, result.Targets, 1)
	assert.Equal(t, "shared:user@example.com:00000b!remote123:remote-folder-1", result.Targets[0].Selector)
	assert.Equal(t, "alice@example.com", result.Targets[0].SharedByEmail)
	assert.Equal(t, sharedOwnerIdentityStatusAvailable, result.Targets[0].OwnerIdentityStatus)
	assert.Empty(t, result.AccountsRequiringAuth)
	assert.Empty(t, result.AccountsDegraded)
}

// Validates: R-3.6.6, R-3.6.7, R-3.7
func TestSharedList_RefreshesIdentityOnceBeforeSharedDiscovery(t *testing.T) {
	setTestDriveHome(t)
	cid := driveid.MustCanonicalID("personal:user@example.com")
	writeTestTokenFile(t, config.DefaultDataDir(), "token_personal_user@example.com.json")
	seedCatalogAccount(t, cid, func(account *config.CatalogAccount) {
		account.UserID = "user-123"
		account.DisplayName = "User Example"
	})

	var meCalls atomic.Int32
	var out bytes.Buffer

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case testGraphMePath:
			meCalls.Add(1)
			writeTestResponse(t, w, `{
				"id":"user-123",
				"displayName":"User Example",
				"mail":"user@example.com"
			}`)
		case testDriveSearchAllPath:
			writeTestResponse(t, w, `{
				"value": [{
					"id":"local-kept",
					"name":"Search Result",
					"folder":{"childCount":1},
					"remoteItem":{
						"id":"remote-folder-1",
						"parentReference":{"driveId":"b!remote123"}
					}
				}]
			}`)
		case testSharedDiscoveryRemoteFolderPath:
			writeTestResponse(t, w, `{
				"id":"remote-folder-1",
				"name":"Search Result",
				"folder":{"childCount":1},
				"parentReference":{"driveId":"00000b!remote123"},
				"shared":{"owner":{"user":{"email":"alice@example.com","displayName":"Alice"}}}
			}`)
		default:
			assert.Failf(t, "unexpected path", "path=%s", r.URL.Path)
		}
	}))
	defer srv.Close()

	cc := &CLIContext{
		Flags:        CLIFlags{JSON: true},
		Logger:       testDriveLogger(t),
		OutputWriter: &out,
		StatusWriter: &out,
		CfgPath:      filepath.Join(t.TempDir(), "config.toml"),
		GraphBaseURL: srv.URL,
	}

	require.NoError(t, runSharedList(context.Background(), cc))

	var parsed sharedListJSONOutput
	require.NoError(t, json.Unmarshal(out.Bytes(), &parsed))
	require.Len(t, parsed.Items, 1)
	assert.Equal(t, "Search Result", parsed.Items[0].Name)
	assert.Equal(t, sharedOwnerIdentityStatusAvailable, parsed.Items[0].OwnerIdentityStatus)
	assert.Equal(t, int32(1), meCalls.Load())
	assert.Empty(t, parsed.AccountsRequiringAuth)
	assert.Empty(t, parsed.AccountsDegraded)
}

// Validates: R-3.6.5, R-3.6.6, R-3.6.7
func TestSharedDiscovery_SearchUnauthorizedReturnsAuthRequired(t *testing.T) {
	setTestDriveHome(t)
	writeTestTokenFile(t, config.DefaultDataDir(), "token_personal_user@example.com.json")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case testGraphMePath:
			writeTestResponse(t, w, `{
				"id":"user-123",
				"displayName":"User Example",
				"mail":"user@example.com"
			}`)
		case testDriveSearchAllPath:
			w.WriteHeader(http.StatusUnauthorized)
			writeTestResponse(t, w, `{"error":{"code":"unauthenticated","message":"token rejected"}}`)
		default:
			assert.Failf(t, "unexpected path", "path=%s", r.URL.Path)
		}
	}))
	defer srv.Close()

	cc := &CLIContext{
		Logger:       testDriveLogger(t),
		OutputWriter: &bytes.Buffer{},
		StatusWriter: &bytes.Buffer{},
		CfgPath:      filepath.Join(t.TempDir(), "config.toml"),
		GraphBaseURL: srv.URL,
	}

	result := discoverSharedTargets(t.Context(), cc, []accountView{{
		Email:                 "user@example.com",
		DisplayName:           "User Example",
		DriveType:             driveid.DriveTypePersonal,
		RepresentativeTokenID: driveid.MustCanonicalID("personal:user@example.com"),
		AuthHealth:            accountAuthHealth{State: authStateReady},
	}})

	assert.Empty(t, result.Targets)
	require.Len(t, result.AccountsRequiringAuth, 1)
	assert.Equal(t, "user@example.com", result.AccountsRequiringAuth[0].Email)
	assert.Equal(t, authReasonSyncAuthRejected, result.AccountsRequiringAuth[0].Reason)
	assert.Empty(t, result.AccountsDegraded)
}

// Validates: R-3.6.5, R-3.6.7
func TestSharedDiscovery_NoRepresentativeTokenReturnsDegraded(t *testing.T) {
	cc := &CLIContext{
		Logger:       testDriveLogger(t),
		OutputWriter: &bytes.Buffer{},
		StatusWriter: &bytes.Buffer{},
		CfgPath:      filepath.Join(t.TempDir(), "config.toml"),
	}

	result := discoverSharedTargets(t.Context(), cc, []accountView{{
		Email:       "user@example.com",
		DisplayName: "User Example",
		DriveType:   driveid.DriveTypePersonal,
		AuthHealth:  accountAuthHealth{State: authStateReady},
	}})

	assert.Empty(t, result.Targets)
	assert.Empty(t, result.AccountsRequiringAuth)
	require.Len(t, result.AccountsDegraded, 1)
	assert.Equal(t, "user@example.com", result.AccountsDegraded[0].Email)
	assert.Equal(t, sharedDiscoveryUnavailableReason, result.AccountsDegraded[0].Reason)
}

// Validates: R-3.6.2, R-3.6.6, R-3.6.7
func TestSharedDiscovery_IgnoresCallerAccountFilter(t *testing.T) {
	setTestDriveHome(t)
	writeTestTokenFile(t, config.DefaultDataDir(), "token_personal_other@example.com.json")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case testDriveSearchAllPath:
			writeTestResponse(t, w, `{
				"value": [{
					"id":"local-kept",
					"name":"Kept Folder",
					"folder":{"childCount":1},
					"remoteItem":{"id":"remote-folder-1","parentReference":{"driveId":"b!remote123"}}
				}]
			}`)
		case testSharedDiscoveryRemoteFolderPath:
			writeTestResponse(t, w, `{
				"id":"remote-folder-1",
				"name":"Kept Folder",
				"folder":{"childCount":1},
				"parentReference":{"driveId":"00000b!remote123"},
				"shared":{"owner":{"user":{"email":"alice@example.com","displayName":"Alice"}}}
			}`)
		default:
			assert.Failf(t, "unexpected path", "path=%s", r.URL.Path)
		}
	}))
	defer srv.Close()

	cc := &CLIContext{
		Flags:        CLIFlags{Account: "user@example.com"},
		Logger:       testDriveLogger(t),
		OutputWriter: &bytes.Buffer{},
		StatusWriter: &bytes.Buffer{},
		CfgPath:      filepath.Join(t.TempDir(), "config.toml"),
		GraphBaseURL: srv.URL,
	}

	result := discoverSharedTargets(t.Context(), cc, []accountView{{
		Email:                 "other@example.com",
		DisplayName:           "Other Example",
		DriveType:             driveid.DriveTypePersonal,
		TokenDriveIDs:         []driveid.CanonicalID{driveid.MustCanonicalID("personal:other@example.com")},
		RepresentativeTokenID: driveid.MustCanonicalID("personal:other@example.com"),
		AuthHealth:            accountAuthHealth{State: authStateReady},
	}})

	require.Len(t, result.Targets, 1)
	assert.Equal(t, "other@example.com", result.Targets[0].AccountEmail)
	assert.Empty(t, result.AccountsRequiringAuth)
	assert.Empty(t, result.AccountsDegraded)
}

// Validates: R-3.6.5, R-3.6.6, R-3.6.7
func TestSharedDiscovery_FallsBackAcrossAccountTokens(t *testing.T) {
	setTestDriveHome(t)
	writeTestTokenFile(t, config.DefaultDataDir(), "token_personal_user@example.com.json")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case testDriveSearchAllPath:
			writeTestResponse(t, w, `{
				"value": [{
					"id":"local-kept",
					"name":"Kept Folder",
					"folder":{"childCount":1},
					"remoteItem":{"id":"remote-folder-1","parentReference":{"driveId":"b!remote123"}}
				}]
			}`)
		case testSharedDiscoveryRemoteFolderPath:
			writeTestResponse(t, w, `{
				"id":"remote-folder-1",
				"name":"Kept Folder",
				"folder":{"childCount":1},
				"parentReference":{"driveId":"00000b!remote123"},
				"shared":{"owner":{"user":{"email":"alice@example.com","displayName":"Alice"}}}
			}`)
		default:
			assert.Failf(t, "unexpected path", "path=%s", r.URL.Path)
		}
	}))
	defer srv.Close()

	cc := &CLIContext{
		Logger:       testDriveLogger(t),
		OutputWriter: &bytes.Buffer{},
		StatusWriter: &bytes.Buffer{},
		CfgPath:      filepath.Join(t.TempDir(), "config.toml"),
		GraphBaseURL: srv.URL,
	}

	result := discoverSharedTargets(t.Context(), cc, []accountView{{
		Email:                 "user@example.com",
		DisplayName:           "User Example",
		DriveType:             driveid.DriveTypeBusiness,
		TokenDriveIDs:         []driveid.CanonicalID{driveid.MustCanonicalID("business:user@example.com"), driveid.MustCanonicalID("personal:user@example.com")},
		RepresentativeTokenID: driveid.MustCanonicalID("business:user@example.com"),
		AuthHealth:            accountAuthHealth{State: authStateReady},
	}})

	require.Len(t, result.Targets, 1)
	assert.Equal(t, "shared:user@example.com:00000b!remote123:remote-folder-1", result.Targets[0].Selector)
	assert.Empty(t, result.AccountsRequiringAuth)
	assert.Empty(t, result.AccountsDegraded)
}

// Validates: R-3.6.7
func TestSharedList_JSONIncludesAuthRequiredWhenSearchUnauthorized(t *testing.T) {
	setTestDriveHome(t)
	writeTestTokenFile(t, config.DefaultDataDir(), "token_personal_user@example.com.json")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case testGraphMePath:
			writeTestResponse(t, w, `{
				"id":"user-123",
				"displayName":"User Example",
				"mail":"user@example.com"
			}`)
		case testDriveSearchAllPath:
			w.WriteHeader(http.StatusUnauthorized)
			writeTestResponse(t, w, `{"error":{"code":"unauthenticated","message":"token rejected"}}`)
		default:
			assert.Failf(t, "unexpected path", "path=%s", r.URL.Path)
		}
	}))
	defer srv.Close()

	var out bytes.Buffer
	cc := &CLIContext{
		Flags:        CLIFlags{JSON: true},
		Logger:       testDriveLogger(t),
		OutputWriter: &out,
		StatusWriter: &out,
		CfgPath:      config.DefaultConfigPath(),
		GraphBaseURL: srv.URL,
	}

	require.NoError(t, runSharedList(context.Background(), cc))

	var parsed sharedListJSONOutput
	require.NoError(t, json.Unmarshal(out.Bytes(), &parsed))
	assert.Empty(t, parsed.Items)
	require.Len(t, parsed.AccountsRequiringAuth, 1)
	assert.Equal(t, "user@example.com", parsed.AccountsRequiringAuth[0].Email)
	assert.Equal(t, authReasonSyncAuthRejected, parsed.AccountsRequiringAuth[0].Reason)
	assert.Empty(t, parsed.AccountsDegraded)
}
