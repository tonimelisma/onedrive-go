package graph

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMe_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/me", r.URL.Path)
		assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{
			"id": "user-abc-123",
			"displayName": "Test User",
			"mail": "test@example.com",
			"userPrincipalName": "test_upn@example.com"
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	user, err := client.Me(context.Background())
	require.NoError(t, err)

	assert.Equal(t, "user-abc-123", user.ID)
	assert.Equal(t, "Test User", user.DisplayName)
	// When mail is present, it takes priority over UPN.
	assert.Equal(t, "test@example.com", user.Email)
}

func TestMe_EmailFallbackToUPN(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Personal accounts often have empty mail field.
		fmt.Fprint(w, `{
			"id": "user-personal",
			"displayName": "Personal User",
			"mail": "",
			"userPrincipalName": "personal@outlook.com"
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	user, err := client.Me(context.Background())
	require.NoError(t, err)

	assert.Equal(t, "user-personal", user.ID)
	assert.Equal(t, "Personal User", user.DisplayName)
	assert.Equal(t, "personal@outlook.com", user.Email)
}

func TestMe_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("request-id", "req-401")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"code":"InvalidAuthenticationToken"}}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, err := client.Me(context.Background())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnauthorized)
}

func TestDrives_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/me/drives", r.URL.Path)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{
			"value": [
				{
					"id": "drive-1",
					"name": "OneDrive",
					"driveType": "personal",
					"owner": {
						"user": {
							"displayName": "Test User"
						}
					},
					"quota": {
						"used": 1073741824,
						"total": 5368709120
					}
				},
				{
					"id": "drive-2",
					"name": "SharePoint Docs",
					"driveType": "documentLibrary",
					"owner": {
						"user": {
							"displayName": "SharePoint"
						}
					},
					"quota": {
						"used": 524288,
						"total": 1099511627776
					}
				}
			]
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	drives, err := client.Drives(context.Background())
	require.NoError(t, err)

	require.Len(t, drives, 2)

	assert.Equal(t, "drive-1", drives[0].ID)
	assert.Equal(t, "OneDrive", drives[0].Name)
	assert.Equal(t, "personal", drives[0].DriveType)
	assert.Equal(t, "Test User", drives[0].OwnerName)
	assert.Equal(t, int64(1073741824), drives[0].QuotaUsed)
	assert.Equal(t, int64(5368709120), drives[0].QuotaTotal)

	assert.Equal(t, "drive-2", drives[1].ID)
	assert.Equal(t, "SharePoint Docs", drives[1].Name)
	assert.Equal(t, "documentLibrary", drives[1].DriveType)
	assert.Equal(t, "SharePoint", drives[1].OwnerName)
	assert.Equal(t, int64(524288), drives[1].QuotaUsed)
	assert.Equal(t, int64(1099511627776), drives[1].QuotaTotal)
}

func TestDrives_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"value": []}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	drives, err := client.Drives(context.Background())
	require.NoError(t, err)

	assert.Empty(t, drives)
}

func TestDrive_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/drives/drive-abc", r.URL.Path)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{
			"id": "drive-abc",
			"name": "My Drive",
			"driveType": "business",
			"owner": {
				"user": {
					"displayName": "Business User"
				}
			},
			"quota": {
				"used": 2147483648,
				"total": 10737418240
			}
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	drive, err := client.Drive(context.Background(), "drive-abc")
	require.NoError(t, err)

	assert.Equal(t, "drive-abc", drive.ID)
	assert.Equal(t, "My Drive", drive.Name)
	assert.Equal(t, "business", drive.DriveType)
	assert.Equal(t, "Business User", drive.OwnerName)
	assert.Equal(t, int64(2147483648), drive.QuotaUsed)
	assert.Equal(t, int64(10737418240), drive.QuotaTotal)
}

func TestDrive_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("request-id", "req-drive-404")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":{"code":"itemNotFound"}}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, err := client.Drive(context.Background(), "nonexistent-drive")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestDrive_NilOwnerAndQuota(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Some drives may not have owner or quota facets.
		fmt.Fprint(w, `{
			"id": "drive-minimal",
			"name": "Minimal Drive",
			"driveType": "personal"
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	drive, err := client.Drive(context.Background(), "drive-minimal")
	require.NoError(t, err)

	assert.Equal(t, "drive-minimal", drive.ID)
	assert.Equal(t, "Minimal Drive", drive.Name)
	assert.Equal(t, "personal", drive.DriveType)
	assert.Empty(t, drive.OwnerName)
	assert.Equal(t, int64(0), drive.QuotaUsed)
	assert.Equal(t, int64(0), drive.QuotaTotal)
}

// --- toUser edge cases ---

func TestToUser_NilMail(t *testing.T) {
	ur := &userResponse{
		ID:          "user-1",
		DisplayName: "Test",
		Mail:        "",
		UPN:         "fallback@example.com",
	}

	user := ur.toUser()
	assert.Equal(t, "fallback@example.com", user.Email)
}

func TestToUser_BothMailAndUPN(t *testing.T) {
	ur := &userResponse{
		ID:          "user-2",
		DisplayName: "Test",
		Mail:        "primary@example.com",
		UPN:         "upn@example.com",
	}

	user := ur.toUser()
	assert.Equal(t, "primary@example.com", user.Email)
}

// --- toDrive edge cases ---

func TestToDrive_NilOwner(t *testing.T) {
	dr := &driveResponse{
		ID:        "d1",
		Name:      "Drive",
		DriveType: "personal",
		Owner:     nil,
		Quota:     &quotaFacet{Used: 100, Total: 200},
	}

	drive := dr.toDrive()
	assert.Empty(t, drive.OwnerName)
	assert.Equal(t, int64(100), drive.QuotaUsed)
	assert.Equal(t, int64(200), drive.QuotaTotal)
}

// --- Organization tests ---

func TestOrganization_Business(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/me/organization", r.URL.Path)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{
			"value": [
				{
					"displayName": "Contoso Ltd"
				}
			]
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	org, err := client.Organization(context.Background())
	require.NoError(t, err)

	assert.Equal(t, "Contoso Ltd", org.DisplayName)
}

func TestOrganization_Personal(t *testing.T) {
	// Personal accounts return an empty array from /me/organization.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"value": []}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	org, err := client.Organization(context.Background())
	require.NoError(t, err)

	assert.Empty(t, org.DisplayName)
}

func TestOrganization_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("request-id", "req-org-401")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"code":"InvalidAuthenticationToken"}}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, err := client.Organization(context.Background())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnauthorized)
}

func TestToDrive_NilQuota(t *testing.T) {
	dr := &driveResponse{
		ID:        "d2",
		Name:      "Drive",
		DriveType: "business",
		Owner: &ownerFacet{User: struct {
			DisplayName string `json:"displayName"`
		}{DisplayName: "Owner"}},
		Quota: nil,
	}

	drive := dr.toDrive()
	assert.Equal(t, "Owner", drive.OwnerName)
	assert.Equal(t, int64(0), drive.QuotaUsed)
	assert.Equal(t, int64(0), drive.QuotaTotal)
}
