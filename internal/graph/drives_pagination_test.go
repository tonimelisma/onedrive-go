package graph

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// twoPageDriveServer serves a drive listing split across two pages, linking the
// first to the second with @odata.nextLink.
func twoPageDriveServer(t *testing.T, secondPagePath string) (*httptest.Server, *int) {
	t.Helper()

	var requests int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++

		w.Header().Set("Content-Type", "application/json")

		if r.URL.Path == secondPagePath {
			writeTestResponse(t, w, `{"value":[{"id":"drive-2","name":"Second","driveType":"business"}]}`)

			return
		}

		writeTestResponse(t, w, fmt.Sprintf(
			`{"value":[{"id":"drive-1","name":"First","driveType":"business"}],`+
				`"@odata.nextLink":%q}`, secondPagePath))
	}))
	t.Cleanup(srv.Close)

	return srv, &requests
}

// Validates: R-6.7.32
//
// A dropped continuation is invisible here rather than loud: the caller gets a
// short list that looks complete, so a drive simply does not exist as far as
// the rest of the program is concerned. The response type did not even carry
// @odata.nextLink, so nothing could have noticed.
func TestDrives_FollowsNextLinkAcrossPages(t *testing.T) {
	t.Parallel()

	srv, requests := twoPageDriveServer(t, "/me/drives-page-2")
	client := newTestClient(t, srv.URL)

	drives, err := client.Drives(t.Context())
	require.NoError(t, err)

	require.Len(t, drives, 2, "both pages of drives must be returned")
	assert.Equal(t, "First", drives[0].Name)
	assert.Equal(t, "Second", drives[1].Name,
		"the drive that only exists on the second page must survive")
	assert.Equal(t, 2, *requests, "the second page must actually be fetched")
}

// Validates: R-6.7.32
func TestSiteDrives_FollowsNextLinkAcrossPages(t *testing.T) {
	t.Parallel()

	srv, requests := twoPageDriveServer(t, "/sites/site-1/drives-page-2")
	client := newTestClient(t, srv.URL)

	drives, err := client.SiteDrives(t.Context(), "site-1")
	require.NoError(t, err)

	require.Len(t, drives, 2, "both pages of site drives must be returned")
	assert.Equal(t, 2, *requests)
}

// Validates: R-6.7.32
//
// A nextLink that points back at itself would otherwise loop until the process
// dies. Delta already guards this; the plain listings did not.
func TestDrives_SelfReferentialNextLinkIsBounded(t *testing.T) {
	t.Parallel()

	var requests int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++

		w.Header().Set("Content-Type", "application/json")
		writeTestResponse(t, w,
			`{"value":[{"id":"drive-loop","name":"Loop","driveType":"business"}],`+
				`"@odata.nextLink":"/me/drives-loop"}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)

	_, err := client.Drives(t.Context())
	require.Error(t, err, "a circular nextLink must terminate with an error, not spin forever")
	assert.Contains(t, err.Error(), "exceeded")
	assert.LessOrEqual(t, requests, defaultMaxListPages+1)
}
