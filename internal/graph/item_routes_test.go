package graph

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// routedItemID has the shape of a Personal item ID: its "!" is a character
// url.PathEscape escapes, so the request line shows whether a route escaped it.
const (
	routedItemID        = "abc!123"
	routedItemIDEscaped = "abc%21123"
)

// itemRouteCall is one client call whose route carries routedItemID.
type itemRouteCall struct {
	route string
	call  func(ctx context.Context, c *Client) error
}

// itemRouteCalls are the calls of every route that names an item by its ID,
// the folder of a listing, a create or a one-name read included.
func itemRouteCalls() []itemRouteCall {
	drive := driveid.New("d")
	content := []byte("x")
	mtime := time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC)

	return []itemRouteCall{
		{route: "get", call: func(ctx context.Context, c *Client) error {
			_, err := c.GetItem(ctx, drive, routedItemID)
			return err
		}},
		{route: "children", call: func(ctx context.Context, c *Client) error {
			_, err := c.ListChildren(ctx, drive, routedItemID)
			return err
		}},
		{route: "child by name", call: func(ctx context.Context, c *Client) error {
			_, err := c.GetChildByName(ctx, drive, routedItemID, "a.txt")
			return err
		}},
		{route: "create folder", call: func(ctx context.Context, c *Client) error {
			_, err := c.CreateFolder(ctx, drive, routedItemID, "f")
			return err
		}},
		{route: "create file", call: func(ctx context.Context, c *Client) error {
			_, err := c.CreateFile(ctx, drive, routedItemID, "a.txt")
			return err
		}},
		{route: "move", call: func(ctx context.Context, c *Client) error {
			_, err := c.MoveItem(ctx, drive, routedItemID, "", "b.txt", time.Time{})
			return err
		}},
		{route: "file system info", call: func(ctx context.Context, c *Client) error {
			_, err := c.UpdateFileSystemInfo(ctx, drive, routedItemID, mtime)
			return err
		}},
		{route: "delete", call: func(ctx context.Context, c *Client) error {
			return c.DeleteItem(ctx, drive, routedItemID)
		}},
		{route: "permanent delete", call: func(ctx context.Context, c *Client) error {
			return c.PermanentDeleteItem(ctx, drive, routedItemID)
		}},
		{route: "permissions", call: func(ctx context.Context, c *Client) error {
			_, err := c.ListItemPermissions(ctx, drive, routedItemID)
			return err
		}},
		{route: "restore", call: func(ctx context.Context, c *Client) error {
			_, err := c.RestoreItem(ctx, drive, routedItemID)
			return err
		}},
		{route: "copy", call: func(ctx context.Context, c *Client) error {
			_, err := c.CopyItem(ctx, drive, routedItemID, "dest", "b.txt")
			return err
		}},
		{route: "upload session", call: func(ctx context.Context, c *Client) error {
			_, err := c.CreateUploadSessionForItem(ctx, drive, routedItemID, "", int64(len(content)), mtime)
			return err
		}},
		{route: "content", call: func(ctx context.Context, c *Client) error {
			_, err := c.SimpleUploadToItem(ctx, drive, routedItemID, "", bytes.NewReader(content), int64(len(content)))
			return err
		}},
		{route: "download", call: func(ctx context.Context, c *Client) error {
			_, err := c.Download(ctx, drive, routedItemID, io.Discard)
			return err
		}},
		{route: "folder delta", call: func(ctx context.Context, c *Client) error {
			_, err := c.DeltaFolder(ctx, drive, routedItemID, "")
			return err
		}},
	}
}

// An item ID goes into every route escaped as one path segment, by the same
// rule as a name: OneDrive's item IDs hold "!", which a route that put the ID
// in as it is sent raw while the path routes sent it as %21.
//
// Validates: R-6.7.8
func TestItemRoutes_EscapeTheItemIDAsOnePathSegment(t *testing.T) {
	t.Parallel()

	for _, tc := range itemRouteCalls() {
		t.Run(tc.route, func(t *testing.T) {
			t.Parallel()

			var mu sync.Mutex
			var uris []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				uris = append(uris, r.RequestURI)
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				writeTestResponse(t, w, `{"error":{"code":"accessDenied","message":"refused"}}`)
			}))
			t.Cleanup(srv.Close)

			require.Error(t, tc.call(t.Context(), newNoRetryTestClient(t, srv.URL)))

			mu.Lock()
			sent := slices.Clone(uris)
			mu.Unlock()
			require.NotEmpty(t, sent)
			for _, uri := range sent {
				assert.Contains(t, uri, "/items/"+routedItemIDEscaped)
				assert.NotContains(t, uri, routedItemID, "the raw ID is never sent")
			}
		})
	}
}
