package graph

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
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

// A dropped continuation would be invisible here rather than loud: the caller
// would get a short list that looks complete, so a drive would simply not
// exist as far as the rest of the program is concerned.
//
// Validates: R-6.7.32
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
// counting the pages in requests. lastPage 0 links on forever: an endless
// chain of new links.
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
// called with arguments the server ignores and returning what it gathered.
// The site search asks for as many sites as the client's page cap, so a
// search that gathers one site a page reaches its limit only on the last
// page the cap allows.
func pagedListingCalls() map[string]func(ctx context.Context, client *Client) (any, error) {
	drive := driveid.New("d")

	return map[string]func(ctx context.Context, client *Client) (any, error){
		"drives":            func(ctx context.Context, c *Client) (any, error) { return c.Drives(ctx) },
		"site drives":       func(ctx context.Context, c *Client) (any, error) { return c.SiteDrives(ctx, "site-1") },
		"site search":       func(ctx context.Context, c *Client) (any, error) { return c.SearchSites(ctx, "*", c.maxListPages) },
		"drive item search": func(ctx context.Context, c *Client) (any, error) { return c.SearchDriveItems(ctx, "q") },
		"item permissions": func(ctx context.Context, c *Client) (any, error) {
			return c.ListItemPermissions(ctx, drive, "item")
		},
		"children": func(ctx context.Context, c *Client) (any, error) { return c.ListChildren(ctx, drive, "p") },
		"children by path": func(ctx context.Context, c *Client) (any, error) {
			return c.ListChildrenByPath(ctx, drive, "Docs")
		},
		"recycle bin": func(ctx context.Context, c *Client) (any, error) { return c.ListRecycleBinItems(ctx, drive) },
	}
}

// everyPagedListingCall is pagedListingCalls and the all-pages delta
// listings, each delta one returning the items and the token it handed back.
func everyPagedListingCall() map[string]func(ctx context.Context, client *Client) (any, error) {
	calls := pagedListingCalls()
	for name, call := range deltaListingCalls() {
		calls[name] = func(ctx context.Context, c *Client) (any, error) {
			items, token, err := call(ctx, c)
			return deltaListing{items: items, token: token}, err
		}
	}

	return calls
}

// Every non-delta paged listing counts the pages it asks for against one
// cap: a listing of exactly that many pages completes, and a continuation
// past it, as an endless chain of new links would give, fails without
// asking for one more page. The cases lower the cap; a client from the
// constructor holds the default one.
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
			_, err := call(t.Context(), client)
			require.NoError(t, err, "a listing of exactly the cap's pages")
			assert.Equal(t, int64(testListPages), whole.Load())

			var endless atomic.Int64
			client = newTestClient(t, pageOnServer(t, 0, &endless).URL)
			client.maxListPages = testListPages
			_, err = call(t.Context(), client)
			require.ErrorContains(t, err, "exceeded")
			assert.Equal(t, int64(testListPages), endless.Load(), "no page past the cap is asked for")
		})
	}
}

// loopedEntryJSON is the one entry on each page of a loopingListing, named
// after its page; every listing reads an ID and a name from it, and the
// listing by path of Docs its parent path.
const loopedEntryJSON = `{"id":%[1]q,"name":%[1]q,"parentReference":{"id":"docs-id","path":"/drive/root:/Docs"}}`

// loopingListing is a Graph server for a listing whose every page holds one
// entry and links to the next, except the page linkFrom, which links back
// to the earlier page linkTo, as a circular continuation would. A link to
// page 1 is the listing's own first request as it was asked for, so a link
// back to it names a page the listing already asked for. It logs the page of
// every request.
type loopingListing struct {
	t        *testing.T
	srv      *httptest.Server
	linkFrom int
	linkTo   int
	// mu guards firstRequest and asked.
	mu           sync.Mutex
	firstRequest string
	asked        []int
}

func newLoopingListing(t *testing.T, linkFrom, linkTo int) *loopingListing {
	t.Helper()

	listing := &loopingListing{t: t, linkFrom: linkFrom, linkTo: linkTo}
	listing.srv = httptest.NewServer(http.HandlerFunc(listing.serve))
	t.Cleanup(listing.srv.Close)

	return listing
}

func (l *loopingListing) serve(w http.ResponseWriter, r *http.Request) {
	page := 1
	if raw := r.URL.Query().Get("page"); raw != "" {
		var err error
		page, err = strconv.Atoi(raw)
		assert.NoError(l.t, err)
	}

	l.mu.Lock()
	if l.firstRequest == "" {
		l.firstRequest = r.RequestURI
	}
	first := l.firstRequest
	l.asked = append(l.asked, page)
	l.mu.Unlock()

	next := page + 1
	if page == l.linkFrom {
		next = l.linkTo
	}
	link := fmt.Sprintf("%s/next-page?page=%d", l.srv.URL, next)
	if next == 1 {
		link = l.srv.URL + first
	}

	entry := fmt.Sprintf(loopedEntryJSON, fmt.Sprintf("page-%d", page))
	w.Header().Set("Content-Type", "application/json")
	writeTestResponsef(l.t, w, `{"value":[%s],"@odata.nextLink":%q}`, entry, link)
}

// asks is the page of every request, in order.
func (l *loopingListing) asks() []int {
	l.mu.Lock()
	defer l.mu.Unlock()

	return append([]int(nil), l.asked...)
}

// Every paged listing, delta enumeration included, fails on a next link back
// to a page it already asked for, before that page is asked for again, and
// hands its caller nothing, a delta listing no token either: a listing that
// gathers every page would otherwise hold the same entries over and over
// until the page cap, and a site search, which stops at its caller's limit,
// would return a site twice. The link back to a later page than the first
// shows that every page asked for is remembered, not only the listing's own
// first request. Both caps are one page above what the longest case asks
// for, so they stop no case, and low only so that a listing that followed
// the link back ends within a few requests.
//
// Validates: R-6.7.32
func TestPagedListings_ALinkBackToAPageAlreadyAskedForFailsTheListing(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		linkFrom int
		linkTo   int
		wantErr  string
		wantAsks []int
	}{
		"a page that links to itself":       {linkFrom: 1, linkTo: 1, wantErr: "looped back from page 1 to page 1", wantAsks: []int{1}},
		"two pages that link to each other": {linkFrom: 2, linkTo: 1, wantErr: "looped back from page 2 to page 1", wantAsks: []int{1, 2}},
		"a later page that links to the one before": {
			linkFrom: 3, linkTo: 2, wantErr: "looped back from page 3 to page 2", wantAsks: []int{1, 2, 3},
		},
	}

	for listingName, call := range everyPagedListingCall() {
		t.Run(listingName, func(t *testing.T) {
			t.Parallel()

			for name, tc := range tests {
				t.Run(name, func(t *testing.T) {
					t.Parallel()

					listing := newLoopingListing(t, tc.linkFrom, tc.linkTo)
					client := newTestClient(t, listing.srv.URL)
					client.maxListPages = testListPages + 1
					client.maxDeltaPages = testListPages + 1

					result, err := call(t.Context(), client)

					assert.Equal(t, tc.wantAsks, listing.asks(), "no page is asked for twice")
					assert.Empty(t, result, "a listing that looped back hands its caller nothing")
					require.ErrorContains(t, err, tc.wantErr)
				})
			}
		})
	}
}

// A link back that would be the first page past the cap reports the cap, not
// the loop: the cap is checked first, so every paged listing, delta
// enumeration included, stops at the cap's last page however that page
// links, and asks for no page past it.
//
// Validates: R-6.7.32
func TestPagedListings_ARepeatAtThePageCapReportsTheCap(t *testing.T) {
	t.Parallel()

	for listingName, call := range everyPagedListingCall() {
		t.Run(listingName, func(t *testing.T) {
			t.Parallel()

			listing := newLoopingListing(t, testListPages, 1)
			client := newTestClient(t, listing.srv.URL)
			client.maxListPages = testListPages
			client.maxDeltaPages = testListPages

			result, err := call(t.Context(), client)

			assert.Equal(t, []int{1, 2, 3}, listing.asks(), "no page past the cap is asked for")
			assert.Empty(t, result)
			require.ErrorContains(t, err, fmt.Sprintf("exceeded %d pages", testListPages))
		})
	}
}

// $top is a page size to Graph, not a result cap: a search that matches more
// sites than one page holds continues with @odata.nextLink, and the site on
// the second page is returned after the first.
//
// Validates: R-6.7.32
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

// The caller's limit caps the result. Once that many sites are in hand the
// walk stops, whatever the server still offers, and the result never exceeds
// the limit.
//
// Validates: R-6.7.32
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
