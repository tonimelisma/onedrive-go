package graph

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// pagedFolderPages is the number of pages a pagedFolder serves, two children
// on each.
const pagedFolderPages = 3

// pagedChildJSON is one child of a pagedFolder, its ID and name the same, in
// the folder p at the path Documents.
const pagedChildJSON = `{"id":%[1]q,"name":%[1]q,"createdDateTime":"2024-01-01T00:00:00Z",` +
	`"lastModifiedDateTime":"2024-01-01T00:00:00Z","parentReference":{"id":"p","driveId":"d","path":"/drives/d/root:/Documents"},` +
	`"file":{"mimeType":"text/plain"}}`

// pagedFolder is a Graph server for one folder of pagedFolderPages pages. It
// logs, in one order, every page asked for ("ask N") and every page handed to
// the caller ("hand <names>"). With checkHandOn set it fails the test when a
// page is asked for before the page before it was handed on.
type pagedFolder struct {
	t           *testing.T
	srv         *httptest.Server
	checkHandOn bool
	// mu guards log, handed and failOnce.
	mu     sync.Mutex
	log    []string
	handed int
	// failOnce answers the page it names with its status, once.
	failOnce map[int]int
	// linkBack makes the page it names link to an earlier page instead of
	// the next, as a circular continuation would. It is set before the
	// listing starts and only read after that.
	linkBack map[int]int
}

func newPagedFolder(t *testing.T, checkHandOn bool) *pagedFolder {
	t.Helper()

	folder := &pagedFolder{t: t, checkHandOn: checkHandOn, failOnce: map[int]int{}}
	folder.srv = httptest.NewServer(http.HandlerFunc(folder.serve))
	t.Cleanup(folder.srv.Close)

	return folder
}

func (f *pagedFolder) serve(w http.ResponseWriter, r *http.Request) {
	page := 1
	if raw := r.URL.Query().Get("page"); raw != "" {
		var err error
		page, err = strconv.Atoi(raw)
		assert.NoError(f.t, err)
	}

	f.mu.Lock()
	f.log = append(f.log, fmt.Sprintf("ask %d", page))
	handed := f.handed
	status := f.failOnce[page]
	delete(f.failOnce, page)
	f.mu.Unlock()

	if f.checkHandOn {
		assert.Equal(f.t, page-1, handed, "page %d is asked for only once page %d was handed on", page, page-1)
	}

	w.Header().Set("Content-Type", "application/json")
	if status != 0 {
		w.WriteHeader(status)
		writeTestResponse(f.t, w, `{"error":{"code":"serviceNotAvailable","message":"try again"}}`)

		return
	}

	next := ""
	if back, ok := f.linkBack[page]; ok {
		next = fmt.Sprintf(`,"@odata.nextLink":%q`, f.pageURL(back))
	} else if page < pagedFolderPages {
		next = fmt.Sprintf(`,"@odata.nextLink":%q`, f.pageURL(page+1))
	}
	writeTestResponsef(f.t, w, `{"value":[%s,%s]%s}`,
		fmt.Sprintf(pagedChildJSON, pagedChildName(page, "a")), fmt.Sprintf(pagedChildJSON, pagedChildName(page, "b")), next)
}

// pageURL is the absolute link to the page of the folder listed by item ID.
// The first page's is the path ListChildrenPages asks for first, so a link
// back to it names a page the listing already asked for.
func (f *pagedFolder) pageURL(page int) string {
	first := fmt.Sprintf("%s/drives/%s/items/p/children?$top=%d", f.srv.URL, driveid.New("d"), listChildrenPageSize)
	if page == 1 {
		return first
	}

	return fmt.Sprintf("%s&page=%d", first, page)
}

// pagedChildName is the name of the child of the page that suffix names.
func pagedChildName(page int, suffix string) string {
	return fmt.Sprintf("page-%d-%s", page, suffix)
}

// hand records page as handed to the caller.
func (f *pagedFolder) hand(page []Item) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.handed++
	f.log = append(f.log, "hand "+strings.Join(itemNames(page), ","))
}

// events is what the server and the caller logged, in order.
func (f *pagedFolder) events() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.log...)
}

func itemNames(items []Item) []string {
	names := make([]string, 0, len(items))
	for i := range items {
		names = append(names, items[i].Name)
	}

	return names
}

// handedPage is the log line of the caller being handed the page.
func handedPage(page int) string {
	return "hand " + pagedChildName(page, "a") + "," + pagedChildName(page, "b")
}

// pagedListings are the two paged forms of a folder listing, by item ID and
// by path.
func pagedListings() map[string]func(ctx context.Context, client *Client, visit ChildrenPageFunc) error {
	return map[string]func(ctx context.Context, client *Client, visit ChildrenPageFunc) error{
		"by item ID": func(ctx context.Context, client *Client, visit ChildrenPageFunc) error {
			return client.ListChildrenPages(ctx, driveid.New("d"), "p", visit)
		},
		"by path": func(ctx context.Context, client *Client, visit ChildrenPageFunc) error {
			return client.ListChildrenByPathPages(ctx, driveid.New("d"), "Documents", visit)
		},
	}
}

// Each page is handed to the caller as it arrives, in the order OneDrive
// returns it, and the next page is asked for only after that: the caller
// never holds more than one page of a folder of any size.
//
// Validates: R-1.1.3, R-6.7.32
func TestListChildrenPages_HandsEachPageOnBeforeAskingForTheNext(t *testing.T) {
	t.Parallel()

	for name, list := range pagedListings() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			folder := newPagedFolder(t, true)
			err := list(t.Context(), newTestClient(t, folder.srv.URL), func(page []Item) bool {
				folder.hand(page)
				return true
			})

			require.NoError(t, err)
			assert.Equal(t, []string{"ask 1", handedPage(1), "ask 2", handedPage(2), "ask 3", handedPage(3)}, folder.events())
		})
	}
}

// A page that fails once is asked for again as that page: the pages before it
// are neither asked for again nor handed on twice.
//
// Validates: R-1.1.3, R-6.8.2
func TestListChildrenPages_RetriesAFailedPageAsThatPage(t *testing.T) {
	t.Parallel()

	folder := newPagedFolder(t, true)
	folder.failOnce[2] = http.StatusServiceUnavailable

	err := newTestClient(t, folder.srv.URL).ListChildrenPages(t.Context(), driveid.New("d"), "p", func(page []Item) bool {
		folder.hand(page)
		return true
	})

	require.NoError(t, err)
	assert.Equal(t, []string{"ask 1", handedPage(1), "ask 2", "ask 2", handedPage(2), "ask 3", handedPage(3)}, folder.events())
}

// A context that ends between two pages ends the listing with the context's
// error, and no further page is asked for.
//
// Validates: R-1.1.3, R-6.3.6
func TestListChildrenPages_CancellationBetweenPagesAsksForNoFurtherPage(t *testing.T) {
	t.Parallel()

	folder := newPagedFolder(t, true)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	err := newTestClient(t, folder.srv.URL).ListChildrenPages(ctx, driveid.New("d"), "p", func(page []Item) bool {
		folder.hand(page)
		cancel()
		return true
	})

	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, []string{"ask 1", handedPage(1)}, folder.events())
}

// A caller that has what it needs stops the listing: no further page is
// asked for, and the listing ends without an error.
//
// Validates: R-1.1.3
func TestListChildrenPages_ACallerThatStopsAsksForNoFurtherPage(t *testing.T) {
	t.Parallel()

	for name, list := range pagedListings() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			folder := newPagedFolder(t, true)
			err := list(t.Context(), newTestClient(t, folder.srv.URL), func(page []Item) bool {
				folder.hand(page)
				return false
			})

			require.NoError(t, err)
			assert.Equal(t, []string{"ask 1", handedPage(1)}, folder.events())
		})
	}
}

// A next link back to a page the listing already asked for fails the listing
// before that page is asked for again, so a caller is never handed the same
// page twice. The cap is lowered only so that a listing that followed the
// link back would end within a few requests instead of thousands.
//
// Validates: R-6.7.32
func TestListChildrenPages_ALinkBackToAPageAlreadyAskedForFailsTheListing(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		linkBack  map[int]int
		wantAsks  []string
		wantPages int
	}{
		"a page that links to itself":       {linkBack: map[int]int{1: 1}, wantAsks: []string{"ask 1"}, wantPages: 1},
		"two pages that link to each other": {linkBack: map[int]int{2: 1}, wantAsks: []string{"ask 1", "ask 2"}, wantPages: 2},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			folder := newPagedFolder(t, false)
			folder.linkBack = tc.linkBack
			client := newTestClient(t, folder.srv.URL)
			client.maxListPages = testListPages

			pages := 0
			err := client.ListChildrenPages(t.Context(), driveid.New("d"), "p", func([]Item) bool {
				pages++
				return true
			})

			require.ErrorContains(t, err, "looped back")
			assert.Equal(t, tc.wantPages, pages, "each page is handed on once")
			assert.Equal(t, tc.wantAsks, folder.events(), "no page is asked for twice")
		})
	}
}

// The slice form is every page, in the order OneDrive returned them.
//
// Validates: R-1.1.3
func TestListChildren_ReturnsEveryPageInListingOrder(t *testing.T) {
	t.Parallel()

	folder := newPagedFolder(t, false)
	items, err := newTestClient(t, folder.srv.URL).ListChildren(t.Context(), driveid.New("d"), "p")

	require.NoError(t, err)
	want := make([]string, 0, 2*pagedFolderPages)
	for page := 1; page <= pagedFolderPages; page++ {
		want = append(want, pagedChildName(page, "a"), pagedChildName(page, "b"))
	}
	assert.Equal(t, want, itemNames(items))
	assert.Equal(t, []string{"ask 1", "ask 2", "ask 3"}, folder.events())
}
