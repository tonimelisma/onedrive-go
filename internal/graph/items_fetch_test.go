package graph

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// childReadParent is the folder every GetChildByName test reads a name in.
const childReadParent = "parent-id"

// childByNameServer answers every request with answer, an item's JSON; the
// function it returns lists each request URI as sent.
func childByNameServer(t *testing.T, answer string) (*httptest.Server, func() []string) {
	t.Helper()

	var mu sync.Mutex
	var uris []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		uris = append(uris, r.RequestURI)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeTestResponse(t, w, answer)
	}))
	t.Cleanup(srv.Close)

	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()

		return slices.Clone(uris)
	}
}

// childJSON is an item named name whose parentReference is parent, a JSON
// object.
func childJSON(name, parent string) string {
	return `{"id":"child-id","name":"` + name + `","file":{},"parentReference":` + parent + `}`
}

// The one name goes out escaped as one path segment and nothing else: a name
// is never decoded, normalized or split, so NFC and NFD spellings are sent as
// their own bytes.
//
// Validates: R-6.7.8
func TestGetChildByName_SendsTheNameAsOneEscapedSegment(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, wantSegment string }{
		{name: "a#b.txt", wantSegment: "a%23b.txt"},
		{name: "100%", wantSegment: "100%25"},
		{name: "my file.txt", wantSegment: "my%20file.txt"},
		{name: "trailing.", wantSegment: "trailing."},
		{name: "caf\xc3\xa9", wantSegment: "caf%C3%A9"},
		{name: "cafe\xcc\x81", wantSegment: "cafe%CC%81"},
		{name: "a?b", wantSegment: "a%3Fb"},
		{name: "a/b", wantSegment: "a%2Fb"},
	} {
		t.Run(tc.wantSegment, func(t *testing.T) {
			t.Parallel()

			srv, uris := childByNameServer(t, childJSON(tc.name, `{"id":"`+childReadParent+`"}`))

			item, err := newTestClient(t, srv.URL).GetChildByName(t.Context(), driveid.New("d"), childReadParent, tc.name)
			require.NoError(t, err)
			assert.Equal(t, tc.name, item.Name)
			assert.Equal(t, []string{"/drives/000000000000000d/items/" + childReadParent + ":/" + tc.wantSegment + ":"}, uris())
		})
	}
}

// OneDrive answers a path read under its own name equivalence, so the item
// it holds under another spelling of the name is the item asked for, and its
// stored spelling is what the caller gets. The parent's ID may come back in
// another letter case, as drive IDs in item ID prefixes do.
//
// Validates: R-2.12.3, R-6.7.22
func TestGetChildByName_AcceptsTheStoredSpellingOfTheSameName(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ asked, stored, parentID string }{
		{asked: "Report.TXT", stored: "report.txt", parentID: childReadParent},
		{asked: "STRASSE", stored: "stra\xc3\x9fe", parentID: childReadParent},
		{asked: "a.txt", stored: "a.txt", parentID: strings.ToUpper(childReadParent)},
	} {
		t.Run(tc.asked, func(t *testing.T) {
			t.Parallel()

			srv, _ := childByNameServer(t, childJSON(tc.stored, `{"id":"`+tc.parentID+`"}`))

			item, err := newTestClient(t, srv.URL).GetChildByName(t.Context(), driveid.New("d"), childReadParent, tc.asked)
			require.NoError(t, err)
			assert.Equal(t, "child-id", item.ID)
			assert.Equal(t, tc.stored, item.Name)
		})
	}
}

// An answer that is not the asked name directly in the asked folder is not
// the item asked for: a path query can resolve elsewhere.
//
// Validates: R-6.7.22, R-2.12.3
func TestGetChildByName_RefusesAnAnswerThatIsNotTheAskedName(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ label, parent, answer string }{
		{label: "another parent", parent: childReadParent, answer: childJSON("a.txt", `{"id":"other-parent"}`)},
		{label: "an unequal name", parent: childReadParent, answer: childJSON("b.txt", `{"id":"`+childReadParent+`"}`)},
		{label: "the root alias with no parent path", parent: driveRootAlias, answer: childJSON("a.txt", `{"id":"real-root"}`)},
		{
			label: "the root alias below the top", parent: driveRootAlias,
			answer: childJSON("a.txt", `{"id":"sub","path":"/drive/root:/Sub"}`),
		},
		{
			label: "the root alias at the top of another drive", parent: driveRootAlias,
			answer: childJSON("a.txt", `{"id":"other-root","driveId":"e","path":"/drives/e/root:"}`),
		},
	} {
		t.Run(tc.label, func(t *testing.T) {
			t.Parallel()

			srv, _ := childByNameServer(t, tc.answer)

			_, err := newTestClient(t, srv.URL).GetChildByName(t.Context(), driveid.New("d"), tc.parent, "a.txt")
			require.ErrorIs(t, err, ErrNotFound)
		})
	}
}

// The drive root's alias names no item, so Graph answers with the root's own
// ID as the parent; the answer's parent path places it at the top of the
// drive, and its parent's drive, in either letter case, is the asked drive.
//
// Validates: R-6.7.22
func TestGetChildByName_PlacesAnAnswerForTheRootAliasByItsParentPath(t *testing.T) {
	t.Parallel()

	srv, uris := childByNameServer(t, childJSON("a.txt", `{"id":"real-root","driveId":"D","path":"/drive/root:"}`))

	item, err := newTestClient(t, srv.URL).GetChildByName(t.Context(), driveid.New("d"), driveRootAlias, "a.txt")
	require.NoError(t, err)
	assert.Equal(t, "child-id", item.ID)
	assert.Equal(t, []string{"/drives/000000000000000d/items/root:/a.txt:"}, uris())
}

// A name the path route cannot carry is refused before any request: one whose
// escaped form is longer than a path-addressed request takes, or one holding
// ':', which ends the path in the route. An empty name names no child.
//
// Validates: R-6.7.8
func TestGetChildByName_RefusesANameTheRouteCannotCarry(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		label, name string
		want        error
	}{
		{label: "too long", name: strings.Repeat("a", maxPathAddressedLength+1), want: ErrNameNotPathAddressable},
		{label: "too long escaped", name: strings.Repeat("#", maxPathAddressedLength/3+1), want: ErrNameNotPathAddressable},
		{label: "a colon", name: "a:b.txt", want: ErrNameNotPathAddressable},
		{label: "empty", name: "", want: ErrInvalidPath},
	} {
		t.Run(tc.label, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Fail(t, "no request may be sent", r.RequestURI)
				w.WriteHeader(http.StatusTeapot)
			}))
			t.Cleanup(srv.Close)

			_, err := newTestClient(t, srv.URL).GetChildByName(t.Context(), driveid.New("d"), childReadParent, tc.name)
			require.ErrorIs(t, err, tc.want)
		})
	}
}

// A path that holds ':' in any of its names is refused before any request, by
// both reads by path: the colon ends the path in a path route (measured on a
// Personal drive: Graph answers 400 for the path cut at the colon), and
// url.PathEscape leaves it as it is. A path long enough to be read in chunks
// is refused the same way.
//
// Validates: R-6.7.22, R-2.11.7
func TestPathReads_RefuseAColonInAnyNameBeforeAnyRequest(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Fail(t, "no request may be sent", r.RequestURI)
		w.WriteHeader(http.StatusTeapot)
	}))
	t.Cleanup(srv.Close)
	client := newTestClient(t, srv.URL)

	for _, tc := range []struct{ label, path string }{
		{label: "a top-level name", path: "co:lon.txt"},
		{label: "the first folder", path: "a:b/c.txt"},
		{label: "a middle folder", path: "a/b:c/d.txt"},
		{label: "the last name", path: "a/b/c:d"},
		{label: "a path read in chunks", path: strings.Repeat("x/", maxPathAddressedLength) + "c:d"},
	} {
		t.Run(tc.label, func(t *testing.T) {
			_, err := client.GetItemByPath(t.Context(), driveid.New("d"), tc.path)
			require.ErrorIs(t, err, ErrNameNotPathAddressable)

			err = client.ListChildrenByPathPages(t.Context(), driveid.New("d"), tc.path, func([]Item) bool { return true })
			require.ErrorIs(t, err, ErrNameNotPathAddressable)
		})
	}
}

// fallbackDriveRoot is the drive root's own ID on the fake of the fallback
// test: the parent an answer for a top-level name carries.
const fallbackDriveRoot = "real-root"

// fallbackServer answers the reads of a drive whose answers carry no parent
// path: each route in answers (the request URI) gets its item JSON, the drive
// root's alias gets the root, and anything else 404. The function it returns
// lists each request URI as sent.
func fallbackServer(t *testing.T, answers map[string]string) (*httptest.Server, func() []string) {
	t.Helper()

	var mu sync.Mutex
	var uris []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		uris = append(uris, r.RequestURI)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		answer, ok := answers[r.RequestURI]
		if r.RequestURI == "/drives/000000000000000d/items/root" {
			answer, ok = `{"id":"`+fallbackDriveRoot+`","name":"root","folder":{},"root":{}}`, true
		}
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			writeTestResponse(t, w, `{"error":{"code":"itemNotFound","message":"no such item"}}`)
			return
		}
		writeTestResponse(t, w, answer)
	}))
	t.Cleanup(srv.Close)

	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()

		return slices.Clone(uris)
	}
}

// pathReadOutcome is what a read by path tells of an answer it holds to the
// path: the item, not there, answers that do not show whether the item is
// there, or an answer that names no folder.
type pathReadOutcome int

const (
	pathReadFound pathReadOutcome = iota + 1
	pathReadNotFound
	pathReadDisagrees
	pathReadUnplaced
)

// requirePathReadOutcome asserts that item and err are the outcome want of a
// read of an item whose ID is a-id. Only "not there" is ErrNotFound: answers
// that disagree, or one that names no folder, leave open where the item is,
// and a caller that took them for an absent item would act on another answer.
// Answers that disagree are ErrPathAnswersDisagree.
func requirePathReadOutcome(t *testing.T, want pathReadOutcome, item *Item, err error) {
	t.Helper()

	switch want {
	case pathReadFound:
		require.NoError(t, err)
		assert.Equal(t, "a-id", item.ID)
		return
	case pathReadNotFound:
		require.ErrorIs(t, err, ErrNotFound)
	case pathReadDisagrees:
		require.ErrorIs(t, err, ErrPathAnswersDisagree)
		require.NotErrorIs(t, err, ErrNotFound)
	case pathReadUnplaced:
		require.ErrorContains(t, err, "without its folder")
		require.NotErrorIs(t, err, ErrNotFound)
	}
	assert.Nil(t, item)
}

// An answer without a parent path is placed by its parent's ID: the parent
// path is read once (the drive root for a top-level name), and the parent's
// ID may come back in another letter case. A top-level answer outside the
// drive root is not the item at the path, and neither is one in another
// folder than the one its own parent path puts at the parent's path. An
// answer in another folder than one held at the parent's path by its name
// alone, or one whose parent's path holds nothing, leaves open whether the
// item is there, and an answer that names no folder cannot be placed: neither
// is "not there".
//
// Validates: R-6.7.22
func TestGetItemByPath_AnswerWithoutParentPathIsPlacedByItsParentsID(t *testing.T) {
	t.Parallel()

	const (
		nestedRoute = "/drives/000000000000000d/root:/Docs/a.txt:"
		parentRoute = "/drives/000000000000000d/root:/Docs:"
		topRoute    = "/drives/000000000000000d/root:/a.txt:"
		rootRoute   = "/drives/000000000000000d/items/root"
		docsFolder  = `{"id":"docs-id","name":"Docs","folder":{},"parentReference":{"id":"` + fallbackDriveRoot + `","path":"/drive/root:"}}`
	)

	for _, tc := range []struct {
		label, path, route, parent, parentRoute string
		// folderAtParentPath replaces docsFolder as the answer at parentRoute.
		folderAtParentPath   string
		noFolderAtParentPath bool
		want                 pathReadOutcome
		wantURIs             []string
	}{
		{
			label: "nested, in its folder", path: "Docs/a.txt", route: nestedRoute, parent: `{"id":"DOCS-ID"}`,
			want: pathReadFound, wantURIs: []string{nestedRoute, parentRoute},
		},
		{
			label: "nested, in another folder than the one at its parent's path", path: "Docs/a.txt", route: nestedRoute,
			parent: `{"id":"other-id"}`, want: pathReadNotFound, wantURIs: []string{nestedRoute, parentRoute},
		},
		{
			label: "nested, in another folder than one held by its name", path: "Docs/a.txt", route: nestedRoute,
			parent:             `{"id":"other-id"}`,
			folderAtParentPath: `{"id":"docs-id","name":"Docs","folder":{},"parentReference":{"id":"` + fallbackDriveRoot + `"}}`,
			want:               pathReadDisagrees, wantURIs: []string{nestedRoute, parentRoute},
		},
		{
			label: "nested, no folder at its parent's path", path: "Docs/a.txt", route: nestedRoute, parent: `{"id":"docs-id"}`,
			noFolderAtParentPath: true, want: pathReadDisagrees, wantURIs: []string{nestedRoute, parentRoute},
		},
		{
			label: "nested, in no folder", path: "Docs/a.txt", route: nestedRoute, parent: `{}`,
			want: pathReadUnplaced, wantURIs: []string{nestedRoute},
		},
		{
			label: "top-level, in the root", path: "a.txt", route: topRoute, parent: `{"id":"` + fallbackDriveRoot + `"}`,
			want: pathReadFound, wantURIs: []string{topRoute, rootRoute},
		},
		{
			label: "top-level, in a folder", path: "a.txt", route: topRoute, parent: `{"id":"docs-id"}`,
			want: pathReadNotFound, wantURIs: []string{topRoute, rootRoute},
		},
	} {
		t.Run(tc.label, func(t *testing.T) {
			t.Parallel()

			answers := map[string]string{tc.route: `{"id":"a-id","name":"A.txt","file":{},"parentReference":` + tc.parent + `}`}
			switch {
			case tc.folderAtParentPath != "":
				answers[parentRoute] = tc.folderAtParentPath
			case !tc.noFolderAtParentPath:
				answers[parentRoute] = docsFolder
			}
			srv, uris := fallbackServer(t, answers)

			item, err := newTestClient(t, srv.URL).GetItemByPath(t.Context(), driveid.New("d"), tc.path)
			requirePathReadOutcome(t, tc.want, item, err)
			assert.Equal(t, tc.wantURIs, uris(), "the answer, then its parent at most once")
		})
	}
}

// pathItemAnswers are fallbackServer's answers for every name on remotePath:
// a folder for each but the last, each in the one before it (the first in the
// drive root), its ID its path's names joined by "-". With withParentPath each
// answer carries its parent path, as a Personal drive's do.
func pathItemAnswers(remotePath string, withParentPath bool) map[string]string {
	answers := map[string]string{}
	parentID, parentPath := fallbackDriveRoot, ""
	names := strings.Split(remotePath, "/")
	for depth, name := range names {
		at := strings.Join(names[:depth+1], "/")
		id := strings.ReplaceAll(at, "/", "-")
		parent := fmt.Sprintf(`{"id":%q}`, parentID)
		if withParentPath {
			parent = fmt.Sprintf(`{"id":%q,"path":%q}`, parentID, "/drive/root:"+parentPath)
		}
		answers["/drives/000000000000000d/root:/"+at+":"] = fmt.Sprintf(`{"id":%q,"name":%q,"folder":{},"parentReference":%s}`, id, name, parent)
		parentID, parentPath = id, parentPath+"/"+name
	}

	return answers
}

// readPlacedByItsParent is what a read by path costs whose answer carries no
// parent path: the read, and one read of its parent.
const readPlacedByItsParent = 2

// A read of a path short enough for one request costs one request when its
// answer carries its parent path, and two when it does not, at any depth: the
// answer, then its parent's ID, read once by the parent's own path (the drive
// root's for a top-level name). The parent is not placed by its own parent in
// turn, which would cost one request more for each folder above it.
//
// Validates: R-6.7.22
func TestGetItemByPath_AShortPathCostsAtMostTwoRequestsAtAnyDepth(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		label, path    string
		withParentPath bool
		wantRequests   int
	}{
		{label: "depth 0 with parent paths", path: "f.txt", withParentPath: true, wantRequests: 1},
		{label: "depth 1 with parent paths", path: "a/f.txt", withParentPath: true, wantRequests: 1},
		{label: "depth 5 with parent paths", path: "a/b/c/d/e/f.txt", withParentPath: true, wantRequests: 1},
		{label: "depth 0 without parent paths", path: "f.txt", wantRequests: readPlacedByItsParent},
		{label: "depth 1 without parent paths", path: "a/f.txt", wantRequests: readPlacedByItsParent},
		{label: "depth 5 without parent paths", path: "a/b/c/d/e/f.txt", wantRequests: readPlacedByItsParent},
	} {
		t.Run(tc.label, func(t *testing.T) {
			t.Parallel()

			srv, uris := fallbackServer(t, pathItemAnswers(tc.path, tc.withParentPath))

			item, err := newTestClient(t, srv.URL).GetItemByPath(t.Context(), driveid.New("d"), tc.path)
			require.NoError(t, err)
			assert.Equal(t, strings.ReplaceAll(tc.path, "/", "-"), item.ID)
			assert.Len(t, uris(), tc.wantRequests, "requests sent: %v", uris())
		})
	}
}

// The parent read that places an answer without a parent path is held to the
// parent's own path as far as its answer allows: by its parent path when it
// carries one, by its name when it does not. Either way it is read once. When
// it answers the folder the item sits in at another path or under another
// name, the answers agree that the item is not at the path. When it answers
// another folder that is not held at the parent's path by its own parent
// path, they do not show whether the item is there.
//
// Validates: R-6.7.22
func TestGetItemByPath_TheParentThatPlacesAnAnswerIsHeldToItsPath(t *testing.T) {
	t.Parallel()

	const (
		route       = "/drives/000000000000000d/root:/Docs/Sub/a.txt:"
		parentRoute = "/drives/000000000000000d/root:/Docs/Sub:"
		answer      = `{"id":"a-id","name":"a.txt","file":{},"parentReference":{"id":"sub-id"}}`
	)

	for _, tc := range []struct {
		label, parent string
		want          pathReadOutcome
	}{
		{
			label:  "the parent at its path",
			parent: `{"id":"sub-id","name":"Sub","folder":{},"parentReference":{"id":"docs-id","path":"/drive/root:/docs"}}`,
			want:   pathReadFound,
		},
		{
			label:  "the parent at another path",
			parent: `{"id":"sub-id","name":"Sub","folder":{},"parentReference":{"id":"other-id","path":"/drive/root:/Other"}}`,
			want:   pathReadNotFound,
		},
		{
			label:  "the parent of its name, no parent path",
			parent: `{"id":"sub-id","name":"SUB","folder":{},"parentReference":{"id":"docs-id"}}`,
			want:   pathReadFound,
		},
		{
			label:  "the parent under another name, no parent path",
			parent: `{"id":"sub-id","name":"Other","folder":{},"parentReference":{"id":"docs-id"}}`,
			want:   pathReadNotFound,
		},
		{
			label:  "another folder at another path",
			parent: `{"id":"other-sub-id","name":"Sub","folder":{},"parentReference":{"id":"other-id","path":"/drive/root:/Other"}}`,
			want:   pathReadDisagrees,
		},
		{
			label:  "another folder of its name, no parent path",
			parent: `{"id":"other-sub-id","name":"Sub","folder":{},"parentReference":{"id":"docs-id"}}`,
			want:   pathReadDisagrees,
		},
	} {
		t.Run(tc.label, func(t *testing.T) {
			t.Parallel()

			srv, uris := fallbackServer(t, map[string]string{route: answer, parentRoute: tc.parent})

			item, err := newTestClient(t, srv.URL).GetItemByPath(t.Context(), driveid.New("d"), "Docs/Sub/a.txt")
			requirePathReadOutcome(t, tc.want, item, err)
			assert.Equal(t, []string{route, parentRoute}, uris(), "the answer, then its parent, once")
		})
	}
}

// The case the parent's placement does not catch, held as it stands: when no
// answer carries a parent path and the path route resolves a/b/c/f.txt into
// another folder's subtree, a folder c in a folder b in a folder x, every
// answer agrees, and the read answers that subtree's file, as a check of the
// leaf name alone does. Placing the parent by its own parent would catch it,
// at a request for each folder above it.
//
// Validates: R-6.7.22
func TestGetItemByPath_AgreeingAnswersFromAnotherSubtreeAreNotCaught(t *testing.T) {
	t.Parallel()

	const root = "/drives/000000000000000d/root:/"
	srv, uris := fallbackServer(t, map[string]string{
		root + "a/b/c/f.txt:": `{"id":"a-id","name":"f.txt","file":{},"parentReference":{"id":"x-b-c"}}`,
		root + "a/b/c:":       `{"id":"x-b-c","name":"c","folder":{},"parentReference":{"id":"x-b"}}`,
		root + "a/b:":         `{"id":"x-b","name":"b","folder":{},"parentReference":{"id":"x"}}`,
		root + "a:":           `{"id":"a","name":"a","folder":{},"parentReference":{"id":"` + fallbackDriveRoot + `"}}`,
	})

	item, err := newTestClient(t, srv.URL).GetItemByPath(t.Context(), driveid.New("d"), "a/b/c/f.txt")
	requirePathReadOutcome(t, pathReadFound, item, err)
	assert.Equal(t, []string{root + "a/b/c/f.txt:", root + "a/b/c:"}, uris())
}

// listedFolderRoute is the folder
// TestListChildrenByPath_HandsOnOnlyChildrenItsAnswersPlaceAtThePath lists,
// read by its path.
const listedFolderRoute = "/drives/000000000000000d/root:/Docs/v1.0:"

// listedFolder is the folder at listedFolderRoute as a read by path answers it.
const listedFolder = `{"id":"v1-id","name":"v1.0","folder":{},"parentReference":{"id":"docs-id","path":"/drive/root:/Docs"}}`

// listedFolderParentRoute is the folder Docs that holds the folder
// listedFolderRoute, read by its path; listedFolderServer answers it without a
// parent path.
const listedFolderParentRoute = "/drives/000000000000000d/root:/Docs:"

// listedFolderServer answers the listing of listedFolderRoute with two pages
// of one child each, both with the parentReference parent, a read of the
// folder itself with folder (404 when folder is empty), and a read of its
// folder Docs with docs-id, which carries no parent path. The function it
// returns lists each request URI as sent.
func listedFolderServer(t *testing.T, parent, folder string) (*httptest.Server, func() []string) {
	t.Helper()

	var mu sync.Mutex
	var uris []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		uris = append(uris, r.RequestURI)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == listedFolderRoute && folder == "":
			w.WriteHeader(http.StatusNotFound)
			writeTestResponse(t, w, `{"error":{"code":"itemNotFound","message":"no such item"}}`)
		case r.URL.Path == listedFolderRoute:
			writeTestResponse(t, w, folder)
		case r.URL.Path == listedFolderParentRoute:
			writeTestResponse(t, w, `{"id":"docs-id","name":"Docs","folder":{},"parentReference":{"id":"root-id"}}`)
		case r.URL.Query().Get("page") == "":
			writeTestResponsef(t, w, `{"value":[{"id":"a-id","name":"a.txt","file":{},"parentReference":%s}],"@odata.nextLink":"http://%s%s?page=2"}`,
				parent, r.Host, r.URL.Path)
		default:
			writeTestResponsef(t, w, `{"value":[{"id":"b-id","name":"b.txt","file":{},"parentReference":%s}]}`, parent)
		}
	}))
	t.Cleanup(srv.Close)

	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()

		return slices.Clone(uris)
	}
}

// A listing by path hands on only the children its answers place at the
// path: the listing's route resolves the path as a read's does, and a read by
// path can answer an item from another path. A child that carries its parent
// path must name the asked folder, and one that names another says the route
// listed another folder: not found. A child without one must sit in the
// folder read at the path, which is read once for the whole listing, placed
// as GetItemByPath places it, and must carry its ID. A child of the folder
// the read answers at another place, or of another folder than the one the
// read's own parent path puts at the path, is not in the folder at the path
// either. A child of another folder than one held at the path by a name
// alone, or one the read answers at another place, or a read of the path that
// finds nothing, leaves open whether the route listed the folder at the path,
// and a child that names no folder cannot be placed. Neither is "not there".
// Each ends the listing before its page is handed on.
//
// Validates: R-6.7.22
func TestListChildrenByPath_HandsOnOnlyChildrenItsAnswersPlaceAtThePath(t *testing.T) {
	t.Parallel()

	const (
		firstPage  = listedFolderRoute + "/children?$top=200"
		secondPage = listedFolderRoute + "/children?page=2"
		// heldByItsName is the folder at listedFolderRoute without a parent
		// path, placed by the folder Docs, which carries none either.
		heldByItsName = `{"id":"v1-id","name":"v1.0","folder":{},"parentReference":{"id":"docs-id"}}`
	)

	for _, tc := range []struct {
		label, parent, folder string
		// noFolder makes the read of the path answer 404.
		noFolder    bool
		wantNames   []string
		wantErr     error
		wantErrText string
		wantURIs    []string
	}{
		{
			label: "children of the folder by its path", parent: `{"id":"v1-id","path":"/drive/root:/docs/V1.0"}`,
			wantNames: []string{"a.txt", "b.txt"}, wantURIs: []string{firstPage, secondPage},
		},
		{
			label: "children of another folder by its path", parent: `{"id":"docs-id","path":"/drive/root:/Docs"}`,
			wantErr: ErrNotFound, wantURIs: []string{firstPage},
		},
		{
			label: "children of the folder by its ID", parent: `{"id":"V1-ID"}`,
			wantNames: []string{"a.txt", "b.txt"}, wantURIs: []string{firstPage, listedFolderRoute, secondPage},
		},
		{
			label: "children of the folder held by its name", parent: `{"id":"v1-id"}`, folder: heldByItsName,
			wantNames: []string{"a.txt", "b.txt"},
			wantURIs:  []string{firstPage, listedFolderRoute, listedFolderParentRoute, secondPage},
		},
		{
			label: "children of another folder than the one at the path by its parent path", parent: `{"id":"docs-id"}`,
			wantErr: ErrNotFound, wantURIs: []string{firstPage, listedFolderRoute},
		},
		{
			label: "children of another folder than one held by its name", parent: `{"id":"docs-id"}`, folder: heldByItsName,
			wantErr: ErrPathAnswersDisagree, wantURIs: []string{firstPage, listedFolderRoute, listedFolderParentRoute},
		},
		{
			label: "children of the folder the read answers at another path", parent: `{"id":"v1-id"}`,
			folder:  `{"id":"v1-id","name":"v2.0","folder":{},"parentReference":{"id":"docs-id","path":"/drive/root:/Docs"}}`,
			wantErr: ErrNotFound, wantURIs: []string{firstPage, listedFolderRoute},
		},
		{
			label: "a read of the path that finds another folder", parent: `{"id":"v1-id"}`,
			folder:  `{"id":"v2-id","name":"v2.0","folder":{},"parentReference":{"id":"docs-id","path":"/drive/root:/Docs"}}`,
			wantErr: ErrPathAnswersDisagree, wantURIs: []string{firstPage, listedFolderRoute},
		},
		{
			label: "a read of the path that finds nothing", parent: `{"id":"v1-id"}`, noFolder: true,
			wantErr: ErrPathAnswersDisagree, wantURIs: []string{firstPage, listedFolderRoute},
		},
		{
			label: "children that name no folder", parent: `{}`,
			wantErrText: "without its folder", wantURIs: []string{firstPage},
		},
		{
			label: "a folder read without its ID", parent: `{"id":"v1-id"}`,
			folder:      `{"name":"v1.0","folder":{},"parentReference":{"id":"docs-id","path":"/drive/root:/Docs"}}`,
			wantErrText: "answered without an item ID", wantURIs: []string{firstPage, listedFolderRoute},
		},
	} {
		t.Run(tc.label, func(t *testing.T) {
			t.Parallel()

			folder := tc.folder
			if folder == "" && !tc.noFolder {
				folder = listedFolder
			}
			srv, uris := listedFolderServer(t, tc.parent, folder)

			var names []string
			err := newTestClient(t, srv.URL).ListChildrenByPathPages(t.Context(), driveid.New("d"), "Docs/v1.0", func(page []Item) bool {
				for i := range page {
					names = append(names, page[i].Name)
				}
				return true
			})
			switch {
			case tc.wantErr != nil:
				require.ErrorIs(t, err, tc.wantErr)
				if !errors.Is(tc.wantErr, ErrNotFound) {
					require.NotErrorIs(t, err, ErrNotFound, "answers that cannot place a child do not say the folder is absent")
				}
			case tc.wantErrText != "":
				require.ErrorContains(t, err, tc.wantErrText)
				require.NotErrorIs(t, err, ErrNotFound, "answers that cannot place a child do not say the folder is absent")
			default:
				require.NoError(t, err)
			}
			assert.Equal(t, tc.wantNames, names)
			assert.Equal(t, tc.wantURIs, uris())
		})
	}
}
