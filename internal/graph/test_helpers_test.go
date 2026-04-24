package graph

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testGraphTimestamp = "2024-01-01T00:00:00Z"

type graphServerOptions struct {
	requestID     string
	contentType   string
	assertRequest func(*http.Request)
	headers       map[string]string
}

func newGraphServer(t *testing.T, status int, body string, opts graphServerOptions) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if opts.assertRequest != nil {
			opts.assertRequest(r)
		}

		if opts.contentType != "" {
			w.Header().Set("Content-Type", opts.contentType)
		}
		if opts.requestID != "" {
			w.Header().Set("request-id", opts.requestID)
		}
		for key, value := range opts.headers {
			w.Header().Set(key, value)
		}

		w.WriteHeader(status)
		if body == "" {
			return
		}

		writeTestResponse(t, w, body)
	}))
}

func writeTestResponse(t *testing.T, w io.Writer, body string) {
	t.Helper()

	_, err := io.WriteString(w, body)
	require.NoError(t, err)
}

func writeTestResponsef(t *testing.T, w io.Writer, format string, args ...any) {
	t.Helper()

	_, err := fmt.Fprintf(w, format, args...)
	require.NoError(t, err)
}

func setTestDirPermissions(t *testing.T, path string, perms os.FileMode) {
	t.Helper()

	require.NoError(t, os.Chmod(path, perms))
}

func newGraphErrorServer(t *testing.T, status int, requestID, code string, assertRequest func(*http.Request)) *httptest.Server {
	t.Helper()

	return newGraphServer(t, status, fmt.Sprintf(`{"error":{"code":%q}}`, code), graphServerOptions{
		requestID:     requestID,
		assertRequest: assertRequest,
	})
}

func assertGraphCallError(t *testing.T, status int, requestID, code string, call func(*Client) error, want error) {
	t.Helper()

	srv := newGraphErrorServer(t, status, requestID, code, nil)
	t.Cleanup(srv.Close)

	client := newTestClient(t, srv.URL)
	err := call(client)
	require.Error(t, err)
	assert.ErrorIs(t, err, want)
}

func assertEmptyGraphSliceCall[T any](t *testing.T, call func(*Client) ([]T, error)) {
	t.Helper()

	srv := newGraphServer(t, http.StatusOK, `{"value": []}`, graphServerOptions{
		contentType: "application/json",
	})
	t.Cleanup(srv.Close)

	client := newTestClient(t, srv.URL)
	items, err := call(client)
	require.NoError(t, err)
	assert.Empty(t, items)
}

func assertInvalidRemotePath(t *testing.T, call func(*Client) error) {
	t.Helper()

	client := newTestClient(t, "http://localhost")
	err := call(client)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidPath)
}

func sharedUser(displayName, email string) *sharedUserFacet {
	return &sharedUserFacet{
		DisplayName: displayName,
		Email:       email,
	}
}

func sharedOwner(displayName, email string) *sharedOwnerFacet {
	return &sharedOwnerFacet{
		User: sharedUser(displayName, email),
	}
}

func sharedIdentity(displayName, email string) *identitySetFacet {
	return &identitySetFacet{
		User: sharedUser(displayName, email),
	}
}

func sharedFolderResponse(itemID, remoteItemID, remoteDriveID string, childCount int) *driveItemResponse {
	return &driveItemResponse{
		ID:                   itemID,
		Name:                 "Shared Folder",
		CreatedDateTime:      testGraphTimestamp,
		LastModifiedDateTime: testGraphTimestamp,
		Folder: &folderFacet{
			ChildCount: childCount,
		},
		RemoteItem: &remoteItemFacet{
			ID: remoteItemID,
			Folder: &folderFacet{
				ChildCount: childCount,
			},
			ParentReference: &parentRef{
				DriveID: remoteDriveID,
			},
		},
	}
}

func withRemoteSharedBy(resp *driveItemResponse, displayName, email string) *driveItemResponse {
	if resp.RemoteItem == nil {
		resp.RemoteItem = &remoteItemFacet{}
	}
	if resp.RemoteItem.Shared == nil {
		resp.RemoteItem.Shared = &sharedFacet{}
	}

	resp.RemoteItem.Shared.SharedBy = sharedOwner(displayName, email)

	return resp
}

func withRemoteOwner(resp *driveItemResponse, displayName, email string) *driveItemResponse {
	if resp.RemoteItem == nil {
		resp.RemoteItem = &remoteItemFacet{}
	}
	if resp.RemoteItem.Shared == nil {
		resp.RemoteItem.Shared = &sharedFacet{}
	}

	resp.RemoteItem.Shared.Owner = sharedOwner(displayName, email)

	return resp
}

func withRemoteCreatedBy(resp *driveItemResponse, displayName, email string) *driveItemResponse {
	if resp.RemoteItem == nil {
		resp.RemoteItem = &remoteItemFacet{}
	}

	resp.RemoteItem.CreatedBy = sharedIdentity(displayName, email)

	return resp
}

func withTopLevelSharedOwner(resp *driveItemResponse, displayName, email string) *driveItemResponse {
	if resp.Shared == nil {
		resp.Shared = &sharedFacet{}
	}

	resp.Shared.Owner = sharedOwner(displayName, email)

	return resp
}
