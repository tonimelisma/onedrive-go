package cli

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
)

const organizationPath = "/me/organization"

func TestDiscoverAccount_PersonalSkipsOrganizationEndpoint(t *testing.T) {
	var organizationCalls atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Path {
		case testGraphMePath:
			writeTestResponse(t, w, `{
				"id": "user-123",
				"displayName": "Personal User",
				"mail": "user@example.com",
				"userPrincipalName": "user@example.com"
			}`)
		case primaryDrivePath:
			writeTestResponse(t, w, `{
				"id": "drive-123",
				"name": "OneDrive",
				"driveType": "personal",
				"quota": {"used": 1, "total": 2}
			}`)
		case organizationPath:
			organizationCalls.Add(1)
			http.Error(w, "personal accounts do not support organization", http.StatusNotFound)
		default:
			assert.Failf(t, "unexpected graph path", "path=%s", r.URL.Path)
			http.Error(w, "unexpected graph path", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	runtime := newDiscoverAccountTestRuntime(srv.URL)

	cid, user, orgName, primaryDriveID, err := discoverAccount(
		t.Context(),
		staticTokenSource{},
		slog.New(slog.DiscardHandler),
		runtime,
	)
	require.NoError(t, err)

	assert.Equal(t, driveid.MustCanonicalID("personal:user@example.com"), cid)
	assert.Equal(t, "user@example.com", user.Email)
	assert.Empty(t, orgName)
	assert.Equal(t, driveid.New("drive-123"), primaryDriveID)
	assert.Equal(t, int64(0), organizationCalls.Load())
}

func TestDiscoverAccount_BusinessFetchesOrganizationName(t *testing.T) {
	var organizationCalls atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Path {
		case testGraphMePath:
			writeTestResponse(t, w, `{
				"id": "user-456",
				"displayName": "Business User",
				"mail": "user@contoso.com",
				"userPrincipalName": "user@contoso.com"
			}`)
		case primaryDrivePath:
			writeTestResponse(t, w, `{
				"id": "drive-456",
				"name": "OneDrive",
				"driveType": "business",
				"quota": {"used": 1, "total": 2}
			}`)
		case organizationPath:
			organizationCalls.Add(1)
			writeTestResponse(t, w, `{
				"value": [
					{"displayName": "Contoso Ltd"}
				]
			}`)
		default:
			assert.Failf(t, "unexpected graph path", "path=%s", r.URL.Path)
			http.Error(w, "unexpected graph path", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	runtime := newDiscoverAccountTestRuntime(srv.URL)

	cid, user, orgName, primaryDriveID, err := discoverAccount(
		t.Context(),
		staticTokenSource{},
		slog.New(slog.DiscardHandler),
		runtime,
	)
	require.NoError(t, err)

	assert.Equal(t, driveid.MustCanonicalID("business:user@contoso.com"), cid)
	assert.Equal(t, "user@contoso.com", user.Email)
	assert.Equal(t, "Contoso Ltd", orgName)
	assert.Equal(t, driveid.New("drive-456"), primaryDriveID)
	assert.Equal(t, int64(1), organizationCalls.Load())
}

func newDiscoverAccountTestRuntime(baseURL string) *driveops.SessionRuntime {
	runtime := driveops.NewSessionRuntime(nil, "test/1.0", slog.New(slog.DiscardHandler))
	runtime.GraphBaseURL = baseURL

	return runtime
}
