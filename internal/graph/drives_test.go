package graph

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
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
	user, err := client.Me(t.Context())
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
	user, err := client.Me(t.Context())
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
	_, err := client.Me(t.Context())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnauthorized)
}

// Validates: R-3.1
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
	drives, err := client.Drives(t.Context())
	require.NoError(t, err)

	require.Len(t, drives, 2)

	assert.Equal(t, driveid.New("drive-1"), drives[0].ID)
	assert.Equal(t, "OneDrive", drives[0].Name)
	assert.Equal(t, "personal", drives[0].DriveType)
	assert.Equal(t, "Test User", drives[0].OwnerName)
	assert.Equal(t, int64(1073741824), drives[0].QuotaUsed)
	assert.Equal(t, int64(5368709120), drives[0].QuotaTotal)

	assert.Equal(t, driveid.New("drive-2"), drives[1].ID)
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
	drives, err := client.Drives(t.Context())
	require.NoError(t, err)

	assert.Empty(t, drives)
}

// Validates: R-3.1
func TestPrimaryDrive_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/me/drive", r.URL.Path)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{
			"id": "f1da660e69bdec82",
			"name": "OneDrive",
			"driveType": "personal",
			"owner": {
				"user": {
					"displayName": "Test User",
					"email": "test@outlook.com"
				}
			},
			"quota": {
				"used": 1560564,
				"total": 5368709120
			}
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	drive, err := client.PrimaryDrive(t.Context())
	require.NoError(t, err)

	assert.Equal(t, driveid.New("f1da660e69bdec82"), drive.ID)
	assert.Equal(t, "OneDrive", drive.Name)
	assert.Equal(t, "personal", drive.DriveType)
	assert.Equal(t, "Test User", drive.OwnerName)
	assert.Equal(t, "test@outlook.com", drive.OwnerEmail)
	assert.Equal(t, int64(1560564), drive.QuotaUsed)
	assert.Equal(t, int64(5368709120), drive.QuotaTotal)
}

func TestPrimaryDrive_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("request-id", "req-primary-500")
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":{"code":"generalException"}}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, err := client.PrimaryDrive(t.Context())
	require.Error(t, err)
}

// Validates: R-3.1
func TestDrive_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/drives/0000000drive-abc", r.URL.Path)

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
	drive, err := client.Drive(t.Context(), driveid.New("drive-abc"))
	require.NoError(t, err)

	assert.Equal(t, driveid.New("drive-abc"), drive.ID)
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
	_, err := client.Drive(t.Context(), driveid.New("nonexistent-drive"))
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
	drive, err := client.Drive(t.Context(), driveid.New("drive-minimal"))
	require.NoError(t, err)

	assert.Equal(t, driveid.New("drive-minimal"), drive.ID)
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
	org, err := client.Organization(t.Context())
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
	org, err := client.Organization(t.Context())
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
	_, err := client.Organization(t.Context())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnauthorized)
}

func TestDrives_DecodeError(t *testing.T) {
	// Verify that Drives returns a decode error when the server returns
	// 200 with invalid JSON.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{not valid json`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, err := client.Drives(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decoding drives response")
}

func TestDrives_Unauthorized(t *testing.T) {
	// Verify that Drives returns ErrUnauthorized on 401.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("request-id", "req-drives-401")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"code":"InvalidAuthenticationToken"}}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, err := client.Drives(t.Context())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnauthorized)
}

// --- SearchSites tests ---

func TestSearchSites_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Contains(t, r.URL.RawQuery, "search=marketing")
		assert.Contains(t, r.URL.RawQuery, "$top=10")

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{
			"value": [
				{
					"id": "site-abc-123",
					"displayName": "Marketing Team",
					"name": "marketing",
					"webUrl": "https://contoso.sharepoint.com/sites/marketing"
				},
				{
					"id": "site-def-456",
					"displayName": "Marketing Archive",
					"name": "marketing-archive",
					"webUrl": "https://contoso.sharepoint.com/sites/marketing-archive"
				}
			]
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	sites, err := client.SearchSites(t.Context(), "marketing", 10)
	require.NoError(t, err)

	require.Len(t, sites, 2)
	assert.Equal(t, "site-abc-123", sites[0].ID)
	assert.Equal(t, "Marketing Team", sites[0].DisplayName)
	assert.Equal(t, "marketing", sites[0].Name)
	assert.Equal(t, "https://contoso.sharepoint.com/sites/marketing", sites[0].WebURL)

	assert.Equal(t, "site-def-456", sites[1].ID)
	assert.Equal(t, "Marketing Archive", sites[1].DisplayName)
}

func TestSearchSites_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"value": []}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	sites, err := client.SearchSites(t.Context(), "nonexistent", 10)
	require.NoError(t, err)
	assert.Empty(t, sites)
}

func TestSearchSites_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("request-id", "req-sites-401")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"code":"InvalidAuthenticationToken"}}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, err := client.SearchSites(t.Context(), "test", 10)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnauthorized)
}

// --- SiteDrives tests ---

func TestSiteDrives_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Contains(t, r.URL.Path, "/sites/site-abc-123/drives")

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{
			"value": [
				{
					"id": "lib-1",
					"name": "Documents",
					"driveType": "documentLibrary",
					"quota": {
						"used": 524288,
						"total": 1099511627776
					}
				},
				{
					"id": "lib-2",
					"name": "Shared Documents",
					"driveType": "documentLibrary"
				}
			]
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	drives, err := client.SiteDrives(t.Context(), "site-abc-123")
	require.NoError(t, err)

	require.Len(t, drives, 2)
	assert.Equal(t, driveid.New("lib-1"), drives[0].ID)
	assert.Equal(t, "Documents", drives[0].Name)
	assert.Equal(t, "documentLibrary", drives[0].DriveType)
	assert.Equal(t, int64(524288), drives[0].QuotaUsed)

	assert.Equal(t, driveid.New("lib-2"), drives[1].ID)
	assert.Equal(t, "Shared Documents", drives[1].Name)
}

func TestSiteDrives_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"value": []}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	drives, err := client.SiteDrives(t.Context(), "site-empty")
	require.NoError(t, err)
	assert.Empty(t, drives)
}

func TestSiteDrives_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("request-id", "req-sitedrives-403")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"error":{"code":"accessDenied"}}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, err := client.SiteDrives(t.Context(), "site-forbidden")
	require.Error(t, err)
}

// --- toSite tests ---

func TestToSite_AllFields(t *testing.T) {
	sr := &siteResponse{
		ID:          "site-1",
		DisplayName: "Marketing",
		Name:        "marketing",
		WebURL:      "https://contoso.sharepoint.com/sites/marketing",
	}

	site := sr.toSite()
	assert.Equal(t, "site-1", site.ID)
	assert.Equal(t, "Marketing", site.DisplayName)
	assert.Equal(t, "marketing", site.Name)
	assert.Equal(t, "https://contoso.sharepoint.com/sites/marketing", site.WebURL)
}

// --- Drives 403 retry tests ---

// Validates: R-6.7.13
func TestDrives_Transient403_Recovers(t *testing.T) {
	// Microsoft Graph occasionally returns transient 403 on /me/drives
	// during token propagation. Drives() should retry and succeed.
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		if attempts <= 2 {
			w.Header().Set("request-id", fmt.Sprintf("req-403-%d", attempts))
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"error":{"code":"accessDenied","message":"Access denied"}}`)

			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"value": [{"id": "drive-1", "name": "OneDrive", "driveType": "personal"}]}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	drives, err := client.Drives(t.Context())
	require.NoError(t, err)

	require.Len(t, drives, 1)
	assert.Equal(t, driveid.New("drive-1"), drives[0].ID)
	assert.Equal(t, 3, attempts, "should have made 3 attempts (2 x 403 + 1 success)")
}

func TestDrives_Permanent403_ExhaustsRetries(t *testing.T) {
	// When all attempts return 403, Drives() should return the error.
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.Header().Set("request-id", fmt.Sprintf("req-perm-403-%d", attempts))
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"error":{"code":"accessDenied","message":"Access denied"}}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, err := client.Drives(t.Context())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrForbidden)
	assert.Equal(t, 3, attempts, "should have exhausted all 3 attempts")
}

func TestDrives_NonForbidden_NoRetry(t *testing.T) {
	// Non-403 errors (e.g. 401) should not be retried.
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.Header().Set("request-id", "req-401")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"code":"InvalidAuthenticationToken"}}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, err := client.Drives(t.Context())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnauthorized)
	assert.Equal(t, 1, attempts, "non-403 errors should not be retried")
}

func TestToDrive_OwnerEmail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{
			"value": [{
				"id": "drive-owner-email",
				"name": "Shared Drive",
				"driveType": "personal",
				"owner": {
					"user": {
						"displayName": "Alice",
						"email": "alice@contoso.com"
					}
				}
			}]
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	drives, err := client.Drives(t.Context())
	require.NoError(t, err)
	require.Len(t, drives, 1)
	assert.Equal(t, "Alice", drives[0].OwnerName)
	assert.Equal(t, "alice@contoso.com", drives[0].OwnerEmail)
}

func TestSearchSites_URLEncodesQuery(t *testing.T) {
	// B-283: Special characters in search queries must be URL-encoded
	// to prevent malformed Graph API requests.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The query "Sales & Marketing" should be URL-encoded in the request.
		assert.Contains(t, r.URL.RawQuery, "search=Sales+%26+Marketing")

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"value": []}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, err := client.SearchSites(t.Context(), "Sales & Marketing", 10)
	require.NoError(t, err)
}

func TestToDrive_NilQuota(t *testing.T) {
	dr := &driveResponse{
		ID:        "d2",
		Name:      "Drive",
		DriveType: "business",
		Owner: &ownerFacet{User: struct {
			DisplayName string `json:"displayName"`
			Email       string `json:"email"`
		}{DisplayName: "Owner", Email: "owner@contoso.com"}},
		Quota: nil,
	}

	drive := dr.toDrive()
	assert.Equal(t, "Owner", drive.OwnerName)
	assert.Equal(t, int64(0), drive.QuotaUsed)
	assert.Equal(t, int64(0), drive.QuotaTotal)
}

// --- SharedWithMe tests ---

func TestSharedWithMe_Success(t *testing.T) {
	// SharedWithMe returns identity under remoteItem.shared (NOT top-level shared)
	// on personal accounts (confirmed via live API testing 2026-03-06).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/me/drive/sharedWithMe", r.URL.Path)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{
			"value": [
				{
					"id": "local-shortcut-1",
					"name": "Shared Folder",
					"size": 0,
					"createdDateTime": "2024-01-01T00:00:00Z",
					"lastModifiedDateTime": "2024-06-01T00:00:00Z",
					"folder": {"childCount": 3},
					"remoteItem": {
						"id": "source-item-1",
						"parentReference": {"driveId": "source-drive-1"},
						"createdBy": {"user": {"email": "alice@example.com", "displayName": "Alice"}},
						"shared": {
							"owner": {"user": {"email": "alice@example.com", "displayName": "Alice"}},
							"sharedBy": {"user": {"email": "alice@example.com", "displayName": "Alice"}}
						}
					}
				},
				{
					"id": "local-shortcut-2",
					"name": "shared-file.docx",
					"size": 2048,
					"createdDateTime": "2024-02-01T00:00:00Z",
					"lastModifiedDateTime": "2024-05-01T00:00:00Z",
					"file": {"mimeType": "application/vnd.openxmlformats-officedocument.wordprocessingml.document"},
					"remoteItem": {
						"id": "source-item-2",
						"parentReference": {"driveId": "source-drive-2"},
						"shared": {
							"owner": {"user": {"email": "bob@example.com", "displayName": "Bob"}}
						}
					}
				}
			]
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	items, err := client.SharedWithMe(t.Context())
	require.NoError(t, err)
	require.Len(t, items, 2)

	// First item: shared folder (sharedBy wins in fallback chain)
	assert.Equal(t, "local-shortcut-1", items[0].ID)
	assert.Equal(t, "Shared Folder", items[0].Name)
	assert.True(t, items[0].IsFolder)
	assert.Equal(t, "source-item-1", items[0].RemoteItemID)
	assert.Equal(t, "source-drive-1", items[0].RemoteDriveID)
	assert.Equal(t, "Alice", items[0].SharedOwnerName)
	assert.Equal(t, "alice@example.com", items[0].SharedOwnerEmail)

	// Second item: shared file (owner used when no sharedBy)
	assert.Equal(t, "local-shortcut-2", items[1].ID)
	assert.Equal(t, "shared-file.docx", items[1].Name)
	assert.False(t, items[1].IsFolder)
	assert.Equal(t, "source-item-2", items[1].RemoteItemID)
	assert.Equal(t, "source-drive-2", items[1].RemoteDriveID)
	assert.Equal(t, "Bob", items[1].SharedOwnerName)
	assert.Equal(t, "bob@example.com", items[1].SharedOwnerEmail)
}

func TestSharedWithMe_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"value": []}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	items, err := client.SharedWithMe(t.Context())
	require.NoError(t, err)
	assert.Empty(t, items)
}

func TestSharedWithMe_Pagination(t *testing.T) {
	// Self-referencing nextLink: the handler needs its own server URL.
	// Use a pointer to hold the server, assigned after creation.
	var page int
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		page++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		if page == 1 {
			fmt.Fprintf(w, `{
				"value": [{
					"id": "item-page1",
					"name": "Page 1 Folder",
					"size": 0,
					"createdDateTime": "2024-01-01T00:00:00Z",
					"lastModifiedDateTime": "2024-01-01T00:00:00Z",
					"folder": {"childCount": 0}
				}],
				"@odata.nextLink": "%s/me/drive/sharedWithMe?$skiptoken=page2"
			}`, srv.URL)
			return
		}

		fmt.Fprint(w, `{
			"value": [{
				"id": "item-page2",
				"name": "Page 2 Folder",
				"size": 0,
				"createdDateTime": "2024-01-01T00:00:00Z",
				"lastModifiedDateTime": "2024-01-01T00:00:00Z",
				"folder": {"childCount": 0}
			}]
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	items, err := client.SharedWithMe(t.Context())
	require.NoError(t, err)
	require.Len(t, items, 2)
	assert.Equal(t, "item-page1", items[0].ID)
	assert.Equal(t, "item-page2", items[1].ID)
	assert.Equal(t, 2, page)
}

func TestSharedWithMe_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("request-id", "req-shared-401")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"code":"InvalidAuthenticationToken"}}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, err := client.SharedWithMe(t.Context())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnauthorized)
}

// --- SearchDriveItems ---

func TestSearchDriveItems_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Contains(t, r.URL.Path, "/me/drive/search")

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{
			"value": [
				{
					"id": "search-item-1",
					"name": "Shared Folder",
					"size": 0,
					"createdDateTime": "2024-01-01T00:00:00Z",
					"lastModifiedDateTime": "2024-06-01T00:00:00Z",
					"folder": {"childCount": 2},
					"remoteItem": {
						"id": "remote-item-1",
						"parentReference": {"driveId": "remote-drive-1"},
						"createdBy": {"user": {"displayName": "Alice"}}
					}
				},
				{
					"id": "search-item-2",
					"name": "My Document.docx",
					"size": 4096,
					"createdDateTime": "2024-03-01T00:00:00Z",
					"lastModifiedDateTime": "2024-07-01T00:00:00Z",
					"file": {"mimeType": "application/vnd.openxmlformats-officedocument.wordprocessingml.document"}
				}
			]
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	items, err := client.SearchDriveItems(t.Context(), "*")
	require.NoError(t, err)
	require.Len(t, items, 2)

	// Shared folder via search — only createdBy.displayName, no email
	assert.Equal(t, "search-item-1", items[0].ID)
	assert.Equal(t, "Shared Folder", items[0].Name)
	assert.True(t, items[0].IsFolder)
	assert.Equal(t, "remote-item-1", items[0].RemoteItemID)
	assert.Equal(t, "remote-drive-1", items[0].RemoteDriveID)
	assert.Equal(t, "Alice", items[0].SharedOwnerName)
	assert.Empty(t, items[0].SharedOwnerEmail) // search doesn't return email

	// Own file — no remoteItem
	assert.Equal(t, "search-item-2", items[1].ID)
	assert.Equal(t, "My Document.docx", items[1].Name)
	assert.False(t, items[1].IsFolder)
	assert.Empty(t, items[1].RemoteItemID)
}

func TestSearchDriveItems_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"value": []}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	items, err := client.SearchDriveItems(t.Context(), "*")
	require.NoError(t, err)
	assert.Empty(t, items)
}

func TestSearchDriveItems_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("request-id", "req-search-401")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"code":"InvalidAuthenticationToken"}}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, err := client.SearchDriveItems(t.Context(), "*")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnauthorized)
}
