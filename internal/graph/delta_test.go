package graph

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDelta_SendsPreferHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify the Prefer header is sent on delta requests.
		assert.Equal(t, "deltashowremoteitemsaliasid", r.Header.Get("Prefer"))

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"value":[],"@odata.deltaLink":"https://example.com/delta?token=abc"}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, err := client.Delta(context.Background(), "d", "")
	require.NoError(t, err)
}

func TestDelta_SinglePage(t *testing.T) {
	var srv *httptest.Server

	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/drives/d/root/delta", r.URL.Path)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{
			"value": [
				{"id":"item-1","name":"file1.txt","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z","parentReference":{"id":"root","driveId":"d"},"file":{"mimeType":"text/plain"}},
				{"id":"item-2","name":"folder1","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z","parentReference":{"id":"root","driveId":"d"},"folder":{"childCount":3}}
			],
			"@odata.deltaLink": "%s/drives/d/root/delta?token=newtoken123"
		}`, srv.URL)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	page, err := client.Delta(context.Background(), "d", "")
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
	var srv *httptest.Server

	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		if !strings.Contains(r.URL.RawQuery, "token=page2") {
			fmt.Fprintf(w, `{
				"value": [
					{"id":"item-1","name":"file1.txt","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z","parentReference":{"id":"root","driveId":"d"}}
				],
				"@odata.nextLink": "%s/drives/d/root/delta?token=page2"
			}`, srv.URL)
		} else {
			fmt.Fprintf(w, `{
				"value": [
					{"id":"item-2","name":"file2.txt","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z","parentReference":{"id":"root","driveId":"d"}}
				],
				"@odata.deltaLink": "%s/drives/d/root/delta?token=final"
			}`, srv.URL)
		}
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)

	page1, err := client.Delta(context.Background(), "d", "")
	require.NoError(t, err)
	assert.Len(t, page1.Items, 1)
	assert.Equal(t, "item-1", page1.Items[0].ID)
	assert.NotEmpty(t, page1.NextLink)
	assert.Empty(t, page1.DeltaLink)

	page2, err := client.Delta(context.Background(), "d", page1.NextLink)
	require.NoError(t, err)
	assert.Len(t, page2.Items, 1)
	assert.Equal(t, "item-2", page2.Items[0].ID)
	assert.Empty(t, page2.NextLink)
	assert.NotEmpty(t, page2.DeltaLink)
}

func TestDelta_EmptyToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/drives/test-drive/root/delta", r.URL.Path)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"value":[],"@odata.deltaLink":"https://example.com/delta?token=abc"}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	page, err := client.Delta(context.Background(), "test-drive", "")
	require.NoError(t, err)

	assert.Empty(t, page.Items)
	assert.NotEmpty(t, page.DeltaLink)
}

func TestDelta_WithToken(t *testing.T) {
	var srv *httptest.Server

	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/drives/d/root/delta", r.URL.Path)
		assert.Equal(t, "prevtoken123", r.URL.Query().Get("token"))

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{
			"value": [
				{"id":"changed-1","name":"updated.txt","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-06-01T00:00:00Z","parentReference":{"id":"root","driveId":"d"}}
			],
			"@odata.deltaLink": "%s/drives/d/root/delta?token=newtoken"
		}`, srv.URL)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	token := srv.URL + "/drives/d/root/delta?token=prevtoken123"
	page, err := client.Delta(context.Background(), "d", token)
	require.NoError(t, err)

	assert.Len(t, page.Items, 1)
	assert.Equal(t, "changed-1", page.Items[0].ID)
}

func TestDelta_Gone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("request-id", "req-gone")
		w.WriteHeader(http.StatusGone)
		fmt.Fprint(w, `{"error":{"code":"resyncRequired","message":"Token expired"}}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, err := client.Delta(context.Background(), "d", "")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrGone)
}

func TestDelta_EmptyPage(t *testing.T) {
	var srv *httptest.Server

	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{
			"value": [],
			"@odata.deltaLink": "%s/drives/d/root/delta?token=emptytoken"
		}`, srv.URL)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	page, err := client.Delta(context.Background(), "d", "")
	require.NoError(t, err)

	assert.Empty(t, page.Items)
	assert.NotEmpty(t, page.DeltaLink)
}

func TestDelta_NormalizesPackages(t *testing.T) {
	var srv *httptest.Server

	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{
			"value": [
				{"id":"file-1","name":"doc.txt","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z","parentReference":{"id":"root","driveId":"d"},"file":{"mimeType":"text/plain"}},
				{"id":"pkg-1","name":"Notebook.one","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z","parentReference":{"id":"root","driveId":"d"},"package":{"type":"oneNote"}}
			],
			"@odata.deltaLink": "%s/drives/d/root/delta?token=abc"
		}`, srv.URL)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	page, err := client.Delta(context.Background(), "d", "")
	require.NoError(t, err)

	assert.Len(t, page.Items, 1)
	assert.Equal(t, "file-1", page.Items[0].ID)
}

func TestDelta_InvalidTokenURL(t *testing.T) {
	client := newTestClient(t, "http://localhost:1234")

	_, err := client.Delta(context.Background(), "d", "http://evil.example.com/drives/d/root/delta?token=bad")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not match base URL")
}

func TestDeltaAll_SinglePage(t *testing.T) {
	var srv *httptest.Server

	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{
			"value": [
				{"id":"item-1","name":"file1.txt","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z","parentReference":{"id":"root","driveId":"d"}},
				{"id":"item-2","name":"file2.txt","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z","parentReference":{"id":"root","driveId":"d"}}
			],
			"@odata.deltaLink": "%s/drives/d/root/delta?token=final"
		}`, srv.URL)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	items, token, err := client.DeltaAll(context.Background(), "d", "")
	require.NoError(t, err)

	assert.Len(t, items, 2)
	assert.Equal(t, "item-1", items[0].ID)
	assert.Equal(t, "item-2", items[1].ID)
	assert.Contains(t, token, "token=final")
}

func TestDeltaAll_MultiPage(t *testing.T) {
	var srv *httptest.Server

	callCount := 0

	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		if !strings.Contains(r.URL.RawQuery, "token=page2") && !strings.Contains(r.URL.RawQuery, "token=page3") {
			fmt.Fprintf(w, `{
				"value": [
					{"id":"item-1","name":"file1.txt","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z","parentReference":{"id":"root","driveId":"d"}}
				],
				"@odata.nextLink": "%s/drives/d/root/delta?token=page2"
			}`, srv.URL)
		} else if strings.Contains(r.URL.RawQuery, "token=page2") {
			fmt.Fprintf(w, `{
				"value": [
					{"id":"item-2","name":"file2.txt","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z","parentReference":{"id":"root","driveId":"d"}}
				],
				"@odata.nextLink": "%s/drives/d/root/delta?token=page3"
			}`, srv.URL)
		} else {
			fmt.Fprintf(w, `{
				"value": [
					{"id":"item-3","name":"file3.txt","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z","parentReference":{"id":"root","driveId":"d"}}
				],
				"@odata.deltaLink": "%s/drives/d/root/delta?token=alldone"
			}`, srv.URL)
		}
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	items, token, err := client.DeltaAll(context.Background(), "d", "")
	require.NoError(t, err)

	assert.Len(t, items, 3)
	assert.Equal(t, "item-1", items[0].ID)
	assert.Equal(t, "item-2", items[1].ID)
	assert.Equal(t, "item-3", items[2].ID)
	assert.Contains(t, token, "token=alldone")
	assert.Equal(t, 3, callCount)
}

func TestDeltaAll_GoneError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("request-id", "req-gone-all")
		w.WriteHeader(http.StatusGone)
		fmt.Fprint(w, `{"error":{"code":"resyncRequired"}}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, _, err := client.DeltaAll(context.Background(), "d", "")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrGone)
}

func TestDeltaAll_MaxPages(t *testing.T) {
	// Override maxDeltaPages to a small value for fast testing.
	origMax := maxDeltaPages
	maxDeltaPages = 3
	defer func() { maxDeltaPages = origMax }()

	var srv *httptest.Server

	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Always return a NextLink, never a DeltaLink — simulates infinite loop.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{
			"value": [
				{"id":"item","name":"file.txt","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z","parentReference":{"id":"root","driveId":"d"}}
			],
			"@odata.nextLink": "%s/drives/d/root/delta?token=next"
		}`, srv.URL)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, _, err := client.DeltaAll(context.Background(), "d", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeded")
	assert.Contains(t, err.Error(), "3")
}

func TestBuildDeltaPath_EmptyToken(t *testing.T) {
	client := newTestClient(t, "http://localhost")
	path, err := client.buildDeltaPath("my-drive", "")
	require.NoError(t, err)
	assert.Equal(t, "/drives/my-drive/root/delta", path)
}

func TestBuildDeltaPath_NonHTTPToken(t *testing.T) {
	client := newTestClient(t, "http://localhost")
	path, err := client.buildDeltaPath("my-drive", "not-a-url")
	require.NoError(t, err)
	assert.Equal(t, "/drives/my-drive/root/delta", path)
}

func TestBuildDeltaPath_FullURLToken(t *testing.T) {
	client := newTestClient(t, "http://localhost")
	path, err := client.buildDeltaPath("d", "http://localhost/drives/d/root/delta?token=abc")
	require.NoError(t, err)
	assert.Equal(t, "/drives/d/root/delta?token=abc", path)
}
