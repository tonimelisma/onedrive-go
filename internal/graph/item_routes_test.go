package graph

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
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
func itemRouteCalls(drive driveid.ID) []itemRouteCall {
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

	for _, tc := range itemRouteCalls(driveid.New("d")) {
		t.Run(tc.route, func(t *testing.T) {
			t.Parallel()

			srv, sent := refusingServer(t)
			require.Error(t, tc.call(t.Context(), newNoRetryTestClient(t, srv.URL)))

			uris := sent()
			require.NotEmpty(t, uris)
			for _, uri := range uris {
				assert.Contains(t, uri, "/items/"+routedItemIDEscaped)
				assert.NotContains(t, uri, routedItemID, "the raw ID is never sent")
			}
		})
	}
}

// routedDriveID has the shape of a Business drive ID, whose "!" a route that
// escaped the drive ID would send as %21; routedSiteID has the shape of a
// SharePoint site ID, whose commas an escaping route would send as %2C.
const (
	routedDriveID = "b!tg9yzw0xyzabc12"
	routedSiteID  = "contoso.sharepoint.com,1111-aaaa,2222-bbbb"
)

// driveRouteCalls are the calls of every route built from a drive ID: each
// route of an item, and the routes of the drive itself.
func driveRouteCalls(drive driveid.ID) []itemRouteCall {
	return append(itemRouteCalls(drive),
		itemRouteCall{route: "drive", call: func(ctx context.Context, c *Client) error {
			_, err := c.Drive(ctx, drive)
			return err
		}},
		itemRouteCall{route: "item by path", call: func(ctx context.Context, c *Client) error {
			_, err := c.GetItemByPath(ctx, drive, "docs/a.txt")
			return err
		}},
		itemRouteCall{route: "children by path", call: func(ctx context.Context, c *Client) error {
			_, err := c.ListChildrenByPath(ctx, drive, "docs")
			return err
		}},
		itemRouteCall{route: "drive delta", call: func(ctx context.Context, c *Client) error {
			_, err := c.Delta(ctx, drive, "")
			return err
		}},
		itemRouteCall{route: "socket.io", call: func(ctx context.Context, c *Client) error {
			_, err := c.SocketIOEndpoint(ctx, drive)
			return err
		}},
		itemRouteCall{route: "recycle bin", call: func(ctx context.Context, c *Client) error {
			_, err := c.ListRecycleBinItems(ctx, drive)
			return err
		}},
	)
}

// refusingServer answers every request 403 and records each request line.
func refusingServer(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()

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

	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()

		return slices.Clone(uris)
	}
}

// A drive ID goes into every route as it is, by one rule: a Business drive
// ID's "!" reaches the request line unescaped in the drive segment, whichever
// route carries it.
//
// Validates: R-6.7.36
func TestDriveRoutes_PutTheDriveIDInAsItIs(t *testing.T) {
	t.Parallel()

	drive := driveid.New(routedDriveID)
	for _, tc := range driveRouteCalls(drive) {
		t.Run(tc.route, func(t *testing.T) {
			t.Parallel()

			srv, sent := refusingServer(t)
			require.Error(t, tc.call(t.Context(), newNoRetryTestClient(t, srv.URL)))

			uris := sent()
			require.NotEmpty(t, uris)
			for _, uri := range uris {
				assert.True(t,
					strings.HasPrefix(uri, "/drives/"+routedDriveID+"/") || uri == "/drives/"+routedDriveID,
					"the drive segment is the raw ID: %s", uri)
			}
		})
	}
}

// A site ID goes into its route as it is: its commas reach the request line
// unescaped.
//
// Validates: R-6.7.36
func TestSiteRoutes_PutTheSiteIDInAsItIs(t *testing.T) {
	t.Parallel()

	srv, sent := refusingServer(t)
	_, err := newNoRetryTestClient(t, srv.URL).SiteDrives(t.Context(), routedSiteID)
	require.Error(t, err)

	uris := sent()
	require.Len(t, uris, 1)
	assert.True(t, strings.HasPrefix(uris[0], "/sites/"+routedSiteID+"/drives?"), uris[0])
}

// A drive or site ID that is empty or holds a character that would change the
// request ("/", "?", "#", "%") is refused before any request is sent, on every
// route.
//
// Validates: R-6.7.36
func TestRoutes_RefuseAnIDThatWouldChangeTheRequest(t *testing.T) {
	t.Parallel()

	refused := []string{"", "b!abc/def0123456", "b!abc?def0123456", "b!abc#def0123456", "b!abc%2fdef01234"}
	for _, id := range refused {
		for _, tc := range driveRouteCalls(driveid.New(id)) {
			t.Run(tc.route+" "+id, func(t *testing.T) {
				t.Parallel()

				srv, sent := refusingServer(t)
				err := tc.call(t.Context(), newNoRetryTestClient(t, srv.URL))
				require.ErrorIs(t, err, errRouteID)
				assert.Empty(t, sent(), "no request is sent for a refused ID")
			})
		}

		t.Run("site drives "+id, func(t *testing.T) {
			t.Parallel()

			srv, sent := refusingServer(t)
			siteID := strings.Replace(id, "b!", "host,", 1)
			_, err := newNoRetryTestClient(t, srv.URL).SiteDrives(t.Context(), siteID)
			require.ErrorIs(t, err, errRouteID)
			assert.Empty(t, sent(), "no request is sent for a refused ID")
		})
	}
}
