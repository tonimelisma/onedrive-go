package graph

import (
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
