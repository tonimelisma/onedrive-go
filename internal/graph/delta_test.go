package graph

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// deltaCapErrorFormat is the error text of a delta listing stopped by its
// page cap, as its callers see it.
const deltaCapErrorFormat = "graph: delta enumeration exceeded %d pages"

// deltaListing is what an all-pages delta listing hands its caller.
type deltaListing struct {
	items []Item
	token string
}

// deltaListingCalls are the client's all-pages delta listings, each called
// with arguments the server ignores. One starts from a saved token, a link to
// the server, so its first request is that link instead of the listing's own
// path.
func deltaListingCalls() map[string]func(ctx context.Context, c *Client) ([]Item, string, error) {
	drive := driveid.New("d")

	return map[string]func(ctx context.Context, c *Client) ([]Item, string, error){
		"drive delta": func(ctx context.Context, c *Client) ([]Item, string, error) {
			return c.DeltaAll(ctx, drive, "")
		},
		"drive delta from a saved token": func(ctx context.Context, c *Client) ([]Item, string, error) {
			return c.DeltaAll(ctx, drive, c.baseURL+"/drives/d/root/delta?token=saved")
		},
		"folder delta": func(ctx context.Context, c *Client) ([]Item, string, error) {
			return c.DeltaFolderAll(ctx, drive, "folder-abc", "")
		},
	}
}

// A delta listing that loops back fails the way one stopped by the page cap
// fails, so its callers classify the two alike: neither error wraps a
// sentinel or a *GraphError, and the callers branch on nothing else. A
// shared-folder observation enumerates again on ErrGone, lists the folder
// instead on ErrNotFound or ErrMethodNotAllowed, records the root's read
// issue on ErrNotFound or ErrForbidden, and takes the context's errors for
// the end of the run; any other error fails a one-shot run and is retried
// with backoff in watch mode.
//
// Validates: R-6.7.32
func TestDeltaListings_ALoopBackFailsAsThePageCapDoes(t *testing.T) {
	t.Parallel()

	branchedOn := []error{ErrGone, ErrNotFound, ErrMethodNotAllowed, ErrForbidden, context.Canceled, context.DeadlineExceeded}

	for name, call := range deltaListingCalls() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var requests atomic.Int64
			capped := newTestClient(t, pageOnServer(t, 0, &requests).URL)
			capped.maxDeltaPages = testListPages
			_, _, capErr := call(t.Context(), capped)
			require.EqualError(t, capErr, fmt.Sprintf(deltaCapErrorFormat, testListPages))

			looping := newTestClient(t, newLoopingListing(t, 1, 1).srv.URL)
			looping.maxDeltaPages = testListPages
			_, _, loopErr := call(t.Context(), looping)
			require.ErrorContains(t, loopErr, "graph: delta enumeration looped back")

			for _, target := range branchedOn {
				assert.Equal(t, errors.Is(capErr, target), errors.Is(loopErr, target), "errors.Is(err, %v)", target)
			}
			var graphErr *GraphError
			assert.Equal(t, errors.As(capErr, &graphErr), errors.As(loopErr, &graphErr), "errors.As(err, *GraphError)")
		})
	}
}

// deltaEndingLinkPath is the path of the delta link deltaEndingServer ends a
// listing with.
const deltaEndingLinkPath = "/drives/d/root/delta?token=next"

// deltaEndingServer is a Graph server whose delta pages link on, each link
// new, until page lastPage answers the delta link that ends the listing,
// counting the pages in requests.
func deltaEndingServer(t *testing.T, lastPage int64, requests *atomic.Int64) *httptest.Server {
	t.Helper()

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		page := requests.Add(1)
		link := fmt.Sprintf(`"@odata.nextLink":%q`, fmt.Sprintf("%s/next-page?page=%d", srv.URL, page+1))
		if page >= lastPage {
			link = fmt.Sprintf(`"@odata.deltaLink":%q`, srv.URL+deltaEndingLinkPath)
		}

		w.Header().Set("Content-Type", "application/json")
		writeTestResponse(t, w, `{"value":[],`+link+`}`)
	}))
	t.Cleanup(srv.Close)

	return srv
}

// A delta listing of exactly the page cap's pages completes: the cap bounds
// the pages asked for, and the delta link on the last of them ends the
// listing with its token.
//
// Validates: R-6.7.32
func TestDeltaListings_AListingOfExactlyThePageCapCompletes(t *testing.T) {
	t.Parallel()

	for name, call := range deltaListingCalls() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var requests atomic.Int64
			srv := deltaEndingServer(t, testListPages, &requests)
			client := newTestClient(t, srv.URL)
			client.maxDeltaPages = testListPages

			_, token, err := call(t.Context(), client)
			require.NoError(t, err)
			assert.Equal(t, int64(testListPages), requests.Load(), "every page up to the cap is asked for once")
			assert.Equal(t, srv.URL+deltaEndingLinkPath, token)
		})
	}
}

// A next link off the client's base URL fails a delta listing before the page
// it names is counted or asked for: the bearer goes nowhere else, and the
// caller is told of the bad link even when the page cap is spent.
//
// Validates: R-6.7.32
func TestDeltaListings_ANextLinkOffTheBaseURLIsRefusedBeforeItIsAsked(t *testing.T) {
	t.Parallel()

	pageCaps := map[string]int{"below the page cap": testListPages, "at the page cap": 1}

	for name, call := range deltaListingCalls() {
		for capName, pageCap := range pageCaps {
			t.Run(name+", "+capName, func(t *testing.T) {
				t.Parallel()

				var requests atomic.Int64
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					requests.Add(1)
					w.Header().Set("Content-Type", "application/json")
					writeTestResponse(t, w, `{"value":[],"@odata.nextLink":"https://elsewhere.invalid/next-page"}`)
				}))
				t.Cleanup(srv.Close)

				client := newTestClient(t, srv.URL)
				client.maxDeltaPages = pageCap

				items, token, err := call(t.Context(), client)
				require.ErrorContains(t, err, "does not match base URL")
				assert.Equal(t, int64(1), requests.Load(), "the link off the base URL is never asked for")
				assert.Nil(t, items)
				assert.Empty(t, token)
			})
		}
	}
}

func TestDelta_SendsPreferHeader(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify the Prefer header is sent on delta requests.
		assert.Equal(t, "deltashowremoteitemsaliasid, Include-Feature=AddToOneDrive", r.Header.Get("Prefer"))

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeTestResponse(t, w, `{"value":[],"@odata.deltaLink":"https://example.com/delta?token=abc"}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, err := client.Delta(t.Context(), driveid.New("d"), "")
	require.NoError(t, err)
}

func TestDelta_SinglePage(t *testing.T) {
	t.Parallel()

	var srv *httptest.Server

	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/drives/000000000000000d/root/delta", r.URL.Path)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeTestResponsef(t, w, `{
			"value": [
				{"id":"item-1","name":"file1.txt","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z","parentReference":{"id":"root","driveId":"d"},"file":{"mimeType":"text/plain"}},
				{"id":"item-2","name":"folder1","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z","parentReference":{"id":"root","driveId":"d"},"folder":{"childCount":3}}
			],
			"@odata.deltaLink": "%s/drives/000000000000000d/root/delta?token=newtoken123"
		}`, srv.URL)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	page, err := client.Delta(t.Context(), driveid.New("d"), "")
	require.NoError(t, err)

	assert.Len(t, page.Items, 2)
	assert.Equal(t, "item-1", page.Items[0].ID)
	assert.Equal(t, "file1.txt", page.Items[0].Name)
	assert.Equal(t, "item-2", page.Items[1].ID)
	assert.True(t, page.Items[1].IsFolder)
	assert.Empty(t, page.NextLink)
	assert.Contains(t, page.DeltaLink, "token=newtoken123")
}

func TestDelta_MultiPage(t *testing.T) {
	t.Parallel()

	var srv *httptest.Server

	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		if !strings.Contains(r.URL.RawQuery, "token=page2") {
			writeTestResponsef(t, w, `{
				"value": [
					{"id":"item-1","name":"file1.txt","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z","parentReference":{"id":"root","driveId":"d"}}
				],
				"@odata.nextLink": "%s/drives/d/root/delta?token=page2"
			}`, srv.URL)
		} else {
			writeTestResponsef(t, w, `{
				"value": [
					{"id":"item-2","name":"file2.txt","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z","parentReference":{"id":"root","driveId":"d"}}
				],
				"@odata.deltaLink": "%s/drives/d/root/delta?token=final"
			}`, srv.URL)
		}
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)

	page1, err := client.Delta(t.Context(), driveid.New("d"), "")
	require.NoError(t, err)
	assert.Len(t, page1.Items, 1)
	assert.Equal(t, "item-1", page1.Items[0].ID)
	assert.NotEmpty(t, page1.NextLink)
	assert.Empty(t, page1.DeltaLink)

	page2, err := client.Delta(t.Context(), driveid.New("d"), page1.NextLink)
	require.NoError(t, err)
	assert.Len(t, page2.Items, 1)
	assert.Equal(t, "item-2", page2.Items[0].ID)
	assert.Empty(t, page2.NextLink)
	assert.NotEmpty(t, page2.DeltaLink)
}

func TestDelta_EmptyToken(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/drives/000000test-drive/root/delta", r.URL.Path)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeTestResponse(t, w, `{"value":[],"@odata.deltaLink":"https://example.com/delta?token=abc"}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	page, err := client.Delta(t.Context(), driveid.New("test-drive"), "")
	require.NoError(t, err)

	assert.Empty(t, page.Items)
	assert.NotEmpty(t, page.DeltaLink)
}

func TestDelta_WithToken(t *testing.T) {
	t.Parallel()

	var srv *httptest.Server

	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/drives/d/root/delta", r.URL.Path)
		assert.Equal(t, "prevtoken123", r.URL.Query().Get("token"))

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeTestResponsef(t, w, `{
			"value": [
				{"id":"changed-1","name":"updated.txt","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-06-01T00:00:00Z","parentReference":{"id":"root","driveId":"d"}}
			],
			"@odata.deltaLink": "%s/drives/d/root/delta?token=newtoken"
		}`, srv.URL)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	token := srv.URL + "/drives/d/root/delta?token=prevtoken123"
	page, err := client.Delta(t.Context(), driveid.New("d"), token)
	require.NoError(t, err)

	assert.Len(t, page.Items, 1)
	assert.Equal(t, "changed-1", page.Items[0].ID)
}

func TestDelta_EmptyPage(t *testing.T) {
	t.Parallel()

	var srv *httptest.Server

	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeTestResponsef(t, w, `{
			"value": [],
			"@odata.deltaLink": "%s/drives/d/root/delta?token=emptytoken"
		}`, srv.URL)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	page, err := client.Delta(t.Context(), driveid.New("d"), "")
	require.NoError(t, err)

	assert.Empty(t, page.Items)
	assert.NotEmpty(t, page.DeltaLink)
}

func TestDelta_KeepsPackages(t *testing.T) {
	t.Parallel()

	var srv *httptest.Server

	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeTestResponsef(t, w, `{
			"value": [
				{"id":"file-1","name":"doc.txt","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z","parentReference":{"id":"root","driveId":"d"},"file":{"mimeType":"text/plain"}},
				{"id":"pkg-1","name":"Notebook.one","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z","parentReference":{"id":"root","driveId":"d"},"package":{"type":"oneNote"}}
			],
			"@odata.deltaLink": "%s/drives/d/root/delta?token=abc"
		}`, srv.URL)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	page, err := client.Delta(t.Context(), driveid.New("d"), "")
	require.NoError(t, err)

	assert.Len(t, page.Items, 2)
	assert.Equal(t, "file-1", page.Items[0].ID)
	assert.True(t, page.Items[1].IsPackage)
}

func TestDelta_InvalidTokenURL(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, "http://localhost:1234")

	_, err := client.Delta(t.Context(), driveid.New("d"), "http://evil.example.com/drives/d/root/delta?token=bad")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not match base URL")
}

func TestDeltaAll_SinglePage(t *testing.T) {
	t.Parallel()

	var srv *httptest.Server

	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeTestResponsef(t, w, `{
			"value": [
				{"id":"item-1","name":"file1.txt","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z","parentReference":{"id":"root","driveId":"d"}},
				{"id":"item-2","name":"file2.txt","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z","parentReference":{"id":"root","driveId":"d"}}
			],
			"@odata.deltaLink": "%s/drives/d/root/delta?token=final"
		}`, srv.URL)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	items, token, err := client.DeltaAll(t.Context(), driveid.New("d"), "")
	require.NoError(t, err)

	assert.Len(t, items, 2)
	assert.Equal(t, "item-1", items[0].ID)
	assert.Equal(t, "item-2", items[1].ID)
	assert.Contains(t, token, "token=final")
}

func TestDeltaAll_MultiPage(t *testing.T) {
	t.Parallel()

	var srv *httptest.Server

	callCount := 0

	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		if !strings.Contains(r.URL.RawQuery, "token=page2") && !strings.Contains(r.URL.RawQuery, "token=page3") {
			writeTestResponsef(t, w, `{
				"value": [
					{"id":"item-1","name":"file1.txt","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z","parentReference":{"id":"root","driveId":"d"}}
				],
				"@odata.nextLink": "%s/drives/d/root/delta?token=page2"
			}`, srv.URL)
		} else if strings.Contains(r.URL.RawQuery, "token=page2") {
			writeTestResponsef(t, w, `{
				"value": [
					{"id":"item-2","name":"file2.txt","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z","parentReference":{"id":"root","driveId":"d"}}
				],
				"@odata.nextLink": "%s/drives/d/root/delta?token=page3"
			}`, srv.URL)
		} else {
			writeTestResponsef(t, w, `{
				"value": [
					{"id":"item-3","name":"file3.txt","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z","parentReference":{"id":"root","driveId":"d"}}
				],
				"@odata.deltaLink": "%s/drives/d/root/delta?token=alldone"
			}`, srv.URL)
		}
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	items, token, err := client.DeltaAll(t.Context(), driveid.New("d"), "")
	require.NoError(t, err)

	assert.Len(t, items, 3)
	assert.Equal(t, "item-1", items[0].ID)
	assert.Equal(t, "item-2", items[1].ID)
	assert.Equal(t, "item-3", items[2].ID)
	assert.Contains(t, token, "token=alldone")
	assert.Equal(t, 3, callCount)
}

func TestDeltaAll_GoneError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("request-id", "req-gone-all")
		w.WriteHeader(http.StatusGone)
		writeTestResponse(t, w, `{"error":{"code":"resyncRequired"}}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, _, err := client.DeltaAll(t.Context(), driveid.New("d"), "")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrGone)
}

func TestDeltaAll_MissingContinuationLinkReturnsError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeTestResponse(t, w, `{
			"value": [
				{"id":"item-1","name":"file1.txt","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z","parentReference":{"id":"root","driveId":"d"}}
			]
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	items, token, err := client.DeltaAll(t.Context(), driveid.New("d"), "")
	require.Error(t, err)
	assert.Nil(t, items)
	assert.Empty(t, token)
	assert.Contains(t, err.Error(), "neither nextLink nor deltaLink")
}

// A drive delta whose next links never end, each one new, stops at the page
// cap: the cap's pages are asked for, the next one is not, and the caller
// gets no items and no token.
//
// Validates: R-6.7.32
func TestDeltaAll_MaxPages(t *testing.T) {
	t.Parallel()

	var requests atomic.Int64
	client := newTestClient(t, pageOnServer(t, 0, &requests).URL)
	client.maxDeltaPages = testListPages

	items, token, err := client.DeltaAll(t.Context(), driveid.New("d"), "")
	require.EqualError(t, err, fmt.Sprintf(deltaCapErrorFormat, testListPages))
	assert.Equal(t, int64(testListPages), requests.Load(), "no page past the cap is asked for")
	assert.Nil(t, items)
	assert.Empty(t, token)
}

// An empty token, or one that is not a URL, starts from the initial path;
// a token Graph returned as a full URL continues from that URL's path.
// Delta and DeltaFolder build their initial paths, which
// TestDelta_EmptyToken and TestDeltaFolder_SinglePage check on the wire.
func TestResolveDeltaToken(t *testing.T) {
	t.Parallel()

	const initialPath = "/drives/000000000000000d/items/folder-abc/delta"
	initial := func() (string, error) { return initialPath, nil }
	client := newTestClient(t, "http://localhost")

	cases := []struct {
		name   string
		cursor string
		want   string
	}{
		{name: "empty token", cursor: "", want: initialPath},
		{name: "token that is not a URL", cursor: "not-a-url", want: initialPath},
		{
			name:   "full URL token",
			cursor: "http://localhost/drives/000000000000000d/items/folder-abc/delta?token=xyz",
			want:   "/drives/000000000000000d/items/folder-abc/delta?token=xyz",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path, err := client.resolveDeltaToken(tc.cursor, initial)
			require.NoError(t, err)
			assert.Equal(t, tc.want, path)
		})
	}
}

// --- DeltaFolder tests ---

func TestDeltaFolder_SinglePage(t *testing.T) {
	t.Parallel()

	var srv *httptest.Server

	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/drives/000000000000000d/items/folder-abc/delta", r.URL.Path)
		// Folder-scoped delta should also send the Prefer header.
		assert.Equal(t, "deltashowremoteitemsaliasid, Include-Feature=AddToOneDrive", r.Header.Get("Prefer"))

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeTestResponsef(t, w, `{
			"value": [
				{"id":"item-1","name":"file1.txt","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z","parentReference":{"id":"folder-abc","driveId":"d"},"file":{"mimeType":"text/plain"}},
				{"id":"item-2","name":"sub","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z","parentReference":{"id":"folder-abc","driveId":"d"},"folder":{"childCount":1}}
			],
			"@odata.deltaLink": "%s/drives/000000000000000d/items/folder-abc/delta?token=newtoken"
		}`, srv.URL)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	page, err := client.DeltaFolder(t.Context(), driveid.New("d"), "folder-abc", "")
	require.NoError(t, err)

	assert.Len(t, page.Items, 2)
	assert.Equal(t, "item-1", page.Items[0].ID)
	assert.Equal(t, "item-2", page.Items[1].ID)
	assert.True(t, page.Items[1].IsFolder)
	assert.Empty(t, page.NextLink)
	assert.Contains(t, page.DeltaLink, "token=newtoken")
}

func TestDeltaFolder_WithToken(t *testing.T) {
	t.Parallel()

	var srv *httptest.Server

	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/drives/d/items/folder-abc/delta", r.URL.Path)
		assert.Equal(t, "prevtoken", r.URL.Query().Get("token"))

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeTestResponsef(t, w, `{
			"value": [
				{"id":"changed-1","name":"updated.txt","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-06-01T00:00:00Z","parentReference":{"id":"folder-abc","driveId":"d"}}
			],
			"@odata.deltaLink": "%s/drives/d/items/folder-abc/delta?token=newtoken"
		}`, srv.URL)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	token := srv.URL + "/drives/d/items/folder-abc/delta?token=prevtoken"
	page, err := client.DeltaFolder(t.Context(), driveid.New("d"), "folder-abc", token)
	require.NoError(t, err)

	assert.Len(t, page.Items, 1)
	assert.Equal(t, "changed-1", page.Items[0].ID)
}

func TestDeltaFolder_Gone(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("request-id", "req-gone-folder")
		w.WriteHeader(http.StatusGone)
		writeTestResponse(t, w, `{"error":{"code":"resyncRequired","message":"Token expired"}}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, err := client.DeltaFolder(t.Context(), driveid.New("d"), "folder-abc", "")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrGone)
}

func TestDeltaFolder_KeepsPackages(t *testing.T) {
	t.Parallel()

	var srv *httptest.Server

	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeTestResponsef(t, w, `{
			"value": [
				{"id":"file-1","name":"doc.txt","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z","parentReference":{"id":"folder-abc","driveId":"d"},"file":{"mimeType":"text/plain"}},
				{"id":"pkg-1","name":"Notebook.one","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z","parentReference":{"id":"folder-abc","driveId":"d"},"package":{"type":"oneNote"}}
			],
			"@odata.deltaLink": "%s/drives/d/items/folder-abc/delta?token=abc"
		}`, srv.URL)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	page, err := client.DeltaFolder(t.Context(), driveid.New("d"), "folder-abc", "")
	require.NoError(t, err)

	assert.Len(t, page.Items, 2)
	assert.Equal(t, "file-1", page.Items[0].ID)
	assert.True(t, page.Items[1].IsPackage)
}

func TestDeltaFolderAll_SinglePage(t *testing.T) {
	t.Parallel()

	var srv *httptest.Server

	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeTestResponsef(t, w, `{
			"value": [
				{"id":"item-1","name":"file1.txt","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z","parentReference":{"id":"folder-abc","driveId":"d"}},
				{"id":"item-2","name":"file2.txt","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z","parentReference":{"id":"folder-abc","driveId":"d"}}
			],
			"@odata.deltaLink": "%s/drives/d/items/folder-abc/delta?token=final"
		}`, srv.URL)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	items, token, err := client.DeltaFolderAll(t.Context(), driveid.New("d"), "folder-abc", "")
	require.NoError(t, err)

	assert.Len(t, items, 2)
	assert.Equal(t, "item-1", items[0].ID)
	assert.Equal(t, "item-2", items[1].ID)
	assert.Contains(t, token, "token=final")
}

func TestDeltaFolderAll_MultiPage(t *testing.T) {
	t.Parallel()

	var srv *httptest.Server

	callCount := 0

	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		if !strings.Contains(r.URL.RawQuery, "token=page2") {
			writeTestResponsef(t, w, `{
				"value": [
					{"id":"item-1","name":"file1.txt","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z","parentReference":{"id":"folder-abc","driveId":"d"}}
				],
				"@odata.nextLink": "%s/drives/d/items/folder-abc/delta?token=page2"
			}`, srv.URL)
		} else {
			writeTestResponsef(t, w, `{
				"value": [
					{"id":"item-2","name":"file2.txt","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z","parentReference":{"id":"folder-abc","driveId":"d"}}
				],
				"@odata.deltaLink": "%s/drives/d/items/folder-abc/delta?token=alldone"
			}`, srv.URL)
		}
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	items, token, err := client.DeltaFolderAll(t.Context(), driveid.New("d"), "folder-abc", "")
	require.NoError(t, err)

	assert.Len(t, items, 2)
	assert.Equal(t, "item-1", items[0].ID)
	assert.Equal(t, "item-2", items[1].ID)
	assert.Contains(t, token, "token=alldone")
	assert.Equal(t, 2, callCount)
}

func TestDeltaFolderAll_MissingContinuationLinkReturnsError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeTestResponse(t, w, `{
			"value": [
				{"id":"item-1","name":"file1.txt","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z","parentReference":{"id":"folder-abc","driveId":"d"}}
			]
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	items, token, err := client.DeltaFolderAll(t.Context(), driveid.New("d"), "folder-abc", "")
	require.Error(t, err)
	assert.Nil(t, items)
	assert.Empty(t, token)
	assert.Contains(t, err.Error(), "neither nextLink nor deltaLink")
}

// A folder delta whose next links never end, each one new, stops at the page
// cap as a drive delta does.
//
// Validates: R-6.7.32
func TestDeltaFolderAll_MaxPages(t *testing.T) {
	t.Parallel()

	var requests atomic.Int64
	client := newTestClient(t, pageOnServer(t, 0, &requests).URL)
	client.maxDeltaPages = testListPages

	items, token, err := client.DeltaFolderAll(t.Context(), driveid.New("d"), "folder-abc", "")
	require.EqualError(t, err, fmt.Sprintf(deltaCapErrorFormat, testListPages))
	assert.Equal(t, int64(testListPages), requests.Load(), "no page past the cap is asked for")
	assert.Nil(t, items)
	assert.Empty(t, token)
}
