package graph

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// twoPageServer serves a listing split across two pages, linking the first
// to the second with @odata.nextLink. firstPageValue and secondPageValue are
// the JSON `value` arrays of each page.
//
// Like real Graph, the nextLink is an absolute URL on the same host as the
// client's base URL. Every request path is recorded so tests can prove the
// second page was requested at the right path rather than at the base URL
// glued to the absolute link.
func twoPageServer(t *testing.T, secondPagePath, firstPageValue, secondPageValue string) (*httptest.Server, *[]string) {
	t.Helper()

	var requestedPaths []string

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedPaths = append(requestedPaths, r.URL.Path)

		w.Header().Set("Content-Type", "application/json")

		if r.URL.Path == secondPagePath {
			writeTestResponse(t, w, fmt.Sprintf(`{"value":%s}`, secondPageValue))

			return
		}

		writeTestResponse(t, w, fmt.Sprintf(`{"value":%s,"@odata.nextLink":%q}`, firstPageValue, srv.URL+secondPagePath))
	}))
	t.Cleanup(srv.Close)

	return srv, &requestedPaths
}

// twoPageDriveServer serves a drive listing split across two pages.
func twoPageDriveServer(t *testing.T, secondPagePath string) (*httptest.Server, *[]string) {
	t.Helper()

	return twoPageServer(t, secondPagePath,
		`[{"id":"drive-1","name":"First","driveType":"business"}]`,
		`[{"id":"drive-2","name":"Second","driveType":"business"}]`)
}

// twoPageSiteServer serves a site search split across two pages.
func twoPageSiteServer(t *testing.T, secondPagePath string) (*httptest.Server, *[]string) {
	t.Helper()

	return twoPageServer(t, secondPagePath,
		`[{"id":"site-1","displayName":"First","name":"first","webUrl":"https://contoso.sharepoint.com/sites/first"}]`,
		`[{"id":"site-2","displayName":"Second","name":"second","webUrl":"https://contoso.sharepoint.com/sites/second"}]`)
}

// Validates: R-6.7.32
//
// A dropped continuation is invisible here rather than loud: the caller gets a
// short list that looks complete, so a drive simply does not exist as far as
// the rest of the program is concerned. The response type did not even carry
// @odata.nextLink, so nothing could have noticed.
func TestDrives_FollowsNextLinkAcrossPages(t *testing.T) {
	t.Parallel()

	srv, requestedPaths := twoPageDriveServer(t, "/me/drives-page-2")
	client := newTestClient(t, srv.URL)

	drives, err := client.Drives(t.Context())
	require.NoError(t, err)

	require.Len(t, drives, 2, "both pages of drives must be returned")
	assert.Equal(t, "First", drives[0].Name)
	assert.Equal(t, "Second", drives[1].Name,
		"the drive that only exists on the second page must survive")
	assert.Equal(t, []string{"/me/drives", "/me/drives-page-2"}, *requestedPaths,
		"the absolute nextLink must be followed at its own path, not appended to the base URL")
}

// Validates: R-6.7.32
func TestSiteDrives_FollowsNextLinkAcrossPages(t *testing.T) {
	t.Parallel()

	srv, requestedPaths := twoPageDriveServer(t, "/sites/site-1/drives-page-2")
	client := newTestClient(t, srv.URL)

	drives, err := client.SiteDrives(t.Context(), "site-1")
	require.NoError(t, err)

	require.Len(t, drives, 2, "both pages of site drives must be returned")
	assert.Equal(t, []string{"/sites/site-1/drives", "/sites/site-1/drives-page-2"}, *requestedPaths)
}

// testListPages is the page cap the bound tests give a client, so the bound
// is reached in a few requests.
const testListPages = 3

// pageOnServer is a Graph server whose every listing answers an empty page
// that links to another while fewer than lastPage pages were asked for,
// counting the pages in requests. lastPage 0 links on forever, as a circular
// nextLink would.
func pageOnServer(t *testing.T, lastPage int64, requests *atomic.Int64) *httptest.Server {
	t.Helper()

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		page := requests.Add(1)
		next := ""
		if lastPage == 0 || page < lastPage {
			next = fmt.Sprintf(`,"@odata.nextLink":%q`, fmt.Sprintf("%s/next-page?page=%d", srv.URL, page+1))
		}

		w.Header().Set("Content-Type", "application/json")
		writeTestResponse(t, w, `{"value":[]`+next+`}`)
	}))
	t.Cleanup(srv.Close)

	return srv
}

// pagedListingCalls are the non-delta paged listings of the client, each
// called with arguments the server ignores.
func pagedListingCalls() map[string]func(ctx context.Context, client *Client) error {
	drive := driveid.New("d")

	return map[string]func(ctx context.Context, client *Client) error{
		"drives":            func(ctx context.Context, c *Client) error { _, err := c.Drives(ctx); return err },
		"site drives":       func(ctx context.Context, c *Client) error { _, err := c.SiteDrives(ctx, "site-1"); return err },
		"site search":       func(ctx context.Context, c *Client) error { _, err := c.SearchSites(ctx, "*", 10); return err },
		"drive item search": func(ctx context.Context, c *Client) error { _, err := c.SearchDriveItems(ctx, "q"); return err },
		"item permissions": func(ctx context.Context, c *Client) error {
			_, err := c.ListItemPermissions(ctx, drive, "item")
			return err
		},
		"children": func(ctx context.Context, c *Client) error { _, err := c.ListChildren(ctx, drive, "p"); return err },
		"children by path": func(ctx context.Context, c *Client) error {
			_, err := c.ListChildrenByPath(ctx, drive, "Docs")
			return err
		},
		"recycle bin": func(ctx context.Context, c *Client) error { _, err := c.ListRecycleBinItems(ctx, drive); return err },
	}
}

// Every non-delta paged listing counts the pages it asks for against one
// cap: a listing of exactly that many pages completes, and a continuation
// past it, which a circular nextLink would follow until the process died,
// fails without asking for one more page. The cases lower the cap; a client
// from the constructor holds the default one.
//
// Validates: R-6.7.32
func TestPagedListings_ThePageCapCountsThePagesAskedFor(t *testing.T) {
	t.Parallel()

	assert.Equal(t, defaultMaxListPages, newTestClient(t, "http://unused").maxListPages,
		"a client from the constructor holds the default page cap")

	for name, call := range pagedListingCalls() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var whole atomic.Int64
			client := newTestClient(t, pageOnServer(t, testListPages, &whole).URL)
			client.maxListPages = testListPages
			require.NoError(t, call(t.Context(), client), "a listing of exactly the cap's pages")
			assert.Equal(t, int64(testListPages), whole.Load())

			var endless atomic.Int64
			client = newTestClient(t, pageOnServer(t, 0, &endless).URL)
			client.maxListPages = testListPages
			require.ErrorContains(t, call(t.Context(), client), "exceeded")
			assert.Equal(t, int64(testListPages), endless.Load(), "no page past the cap is asked for")
		})
	}
}

// Validates: R-6.7.32
//
// $top is a page size to Graph, not a result cap: a search that matches more
// sites than one page holds continues with @odata.nextLink, and a site on the
// second page used to vanish silently.
func TestSearchSites_FollowsNextLinkAcrossPages(t *testing.T) {
	t.Parallel()

	srv, requestedPaths := twoPageSiteServer(t, "/sites-page-2")
	client := newTestClient(t, srv.URL)

	sites, err := client.SearchSites(t.Context(), "marketing", 10)
	require.NoError(t, err)

	require.Len(t, sites, 2, "both pages of sites must be returned")
	assert.Equal(t, "site-1", sites[0].ID)
	assert.Equal(t, "site-2", sites[1].ID, "the site that only exists on the second page must survive")
	assert.Equal(t, []string{"/sites", "/sites-page-2"}, *requestedPaths,
		"the absolute nextLink must be followed at its own path, not appended to the base URL")
}

// Validates: R-6.7.32
//
// The caller's limit caps the result. Once that many sites are in hand the
// walk stops, whatever the server still offers, and the result never exceeds
// the limit.
func TestSearchSites_StopsFollowingAtTheLimit(t *testing.T) {
	t.Parallel()

	var requests int

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++

		w.Header().Set("Content-Type", "application/json")
		writeTestResponse(t, w, fmt.Sprintf(
			`{"value":[{"id":"site-a-%d","displayName":"A","name":"a","webUrl":"https://contoso.sharepoint.com/sites/a"},`+
				`{"id":"site-b-%d","displayName":"B","name":"b","webUrl":"https://contoso.sharepoint.com/sites/b"}],`+
				`"@odata.nextLink":%q}`, requests, requests, srv.URL+"/sites-more"))
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)

	sites, err := client.SearchSites(t.Context(), "*", 3)
	require.NoError(t, err)
	assert.Len(t, sites, 3, "the result is capped at the limit")
	assert.Equal(t, 2, requests, "the walk stops once the limit is reached")
}
