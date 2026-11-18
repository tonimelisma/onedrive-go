package driveops

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// namedChild is one item a folder of namedFolders holds.
type namedChild struct {
	id, name string
	isFolder bool
}

// namedFolders is a Graph fake of folders by item ID, each holding children.
// A read of one name below a folder (items/{id}:/{name}:) answers the child of
// that name, letter case aside, or 404 when the folder is one of readMisses; a
// listing (items/{id}/children) serves the folder's children one to a page,
// linked by next links. It counts the reads and the listing pages asked for.
type namedFolders struct {
	t            *testing.T
	children     map[string][]namedChild
	readMisses   map[string]bool
	reads, pages atomic.Int32
}

func (f *namedFolders) serve(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/drives/abcdef0123456789/items/")
	if parentID, route, isRead := strings.Cut(rest, ":/"); isRead {
		f.reads.Add(1)
		name := strings.TrimSuffix(route, ":")
		for _, child := range f.children[parentID] {
			if !f.readMisses[parentID] && strings.EqualFold(child.name, name) {
				writeFolderConflictJSON(f.t, w, f.answer(parentID, child))
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		writeTestResponsef(f.t, w, `{"error":{"code":"itemNotFound"}}`)

		return
	}

	parentID := strings.TrimSuffix(rest, "/children")
	page := 1
	if raw := r.URL.Query().Get("page"); raw != "" {
		var err error
		page, err = strconv.Atoi(raw)
		assert.NoError(f.t, err)
	}
	f.pages.Add(1)

	children := f.children[parentID]
	listing := map[string]any{"value": []any{}}
	if page <= len(children) {
		listing["value"] = []any{f.answer(parentID, children[page-1])}
	}
	if page < len(children) {
		listing["@odata.nextLink"] = fmt.Sprintf("http://%s%s?page=%d", r.Host, r.URL.Path, page+1)
	}
	writeFolderConflictJSON(f.t, w, listing)
}

// answer is child as Graph answers it, in the folder parentID.
func (f *namedFolders) answer(parentID string, child namedChild) map[string]any {
	item := map[string]any{"id": child.id, "name": child.name, "parentReference": map[string]any{"id": parentID}}
	if child.isFolder {
		item["folder"] = map[string]any{}
	} else {
		item["file"] = map[string]any{}
	}

	return item
}

// lookupParent is the folder the upload-lookup test looks a name up in.
const lookupParent = "parent-id"

// namePage is the listing page of lookupParent that holds the name looked up.
const namePage = 2

// An upload whose answer did not carry its item looks the item up by one read
// of its name. The folder is listed only when that read cannot settle the
// name, a page at a time, and no further than the page that holds it.
//
// Validates: R-5.5.2
func TestLookupUploadTarget_ReadsTheNameAndListsOnlyWhenTheReadCannotSettleIt(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		label, name string
		readMisses  bool
		wantReads   int32
		wantPages   int32
	}{
		{label: "the read answers", name: "a.txt", wantReads: 1},
		{label: "the read finds nothing", name: "a.txt", readMisses: true, wantReads: 1, wantPages: namePage},
		{label: "a name the read cannot carry", name: "a:b.txt", wantPages: namePage},
	} {
		t.Run(tc.label, func(t *testing.T) {
			t.Parallel()

			fake := &namedFolders{
				t: t,
				children: map[string][]namedChild{lookupParent: {
					{id: "other-1", name: "b.txt"}, {id: "file-id", name: tc.name}, {id: "other-2", name: "c.txt"},
				}},
				readMisses: map[string]bool{lookupParent: tc.readMisses},
			}
			client := newTestSession(t, http.HandlerFunc(fake.serve)).Meta

			item, err := newTestTM(&tmSimpleDownloader{}, client, nil).lookupUploadTarget(t.Context(),
				uploadTarget{driveID: driveid.New("abcdef0123456789"), parentID: lookupParent, name: tc.name})
			require.NoError(t, err)
			assert.Equal(t, "file-id", item.ID)
			assert.Equal(t, tc.wantReads, fake.reads.Load())
			assert.Equal(t, tc.wantPages, fake.pages.Load())
		})
	}
}

// A path below a mounted folder is resolved by one read of each name, from
// the mount's root item down. A folder is listed only for a name the read
// cannot settle, and no further than the page that holds it.
//
// Validates: R-6.7.22
func TestMountSession_ResolveItem_MountRootReadsEachNameOnce(t *testing.T) {
	t.Parallel()

	const mountRoot = "mount-root-item"

	for _, tc := range []struct {
		label      string
		readMisses map[string]bool
		wantReads  int32
		wantPages  int32
	}{
		{label: "every read answers", wantReads: 2},
		{label: "the root's read finds nothing", readMisses: map[string]bool{mountRoot: true}, wantReads: 2, wantPages: namePage},
	} {
		t.Run(tc.label, func(t *testing.T) {
			t.Parallel()

			fake := &namedFolders{
				t: t,
				children: map[string][]namedChild{
					mountRoot: {
						{id: "other-1", name: "Other", isFolder: true},
						{id: "folder-id", name: "Documents", isFolder: true},
						{id: "other-2", name: "More", isFolder: true},
					},
					"folder-id": {{id: "file-id", name: "report.docx"}},
				},
				readMisses: tc.readMisses,
			}
			session := NewMountSession(newTestSession(t, http.HandlerFunc(fake.serve)), mountRoot)

			item, err := session.ResolveItem(t.Context(), "Documents/report.docx")
			require.NoError(t, err)
			assert.Equal(t, "file-id", item.ID)
			assert.Equal(t, tc.wantReads, fake.reads.Load())
			assert.Equal(t, tc.wantPages, fake.pages.Load())
		})
	}
}

// A listing that looks for a name stops at the page that holds it as spelled.
// A page that holds only another spelling OneDrive treats as the same does not
// stop it: a later page could hold the exact spelling, which is then the
// item, and failing that the first other spelling is.
//
// Validates: R-2.12.3
func TestFindExistingChild_TheListingPrefersTheNameAsSpelled(t *testing.T) {
	t.Parallel()

	const everyPage = 3

	for _, tc := range []struct {
		label     string
		children  []namedChild
		wantID    string
		wantPages int32
	}{
		{
			label:    "the exact spelling on a later page",
			children: []namedChild{{id: "folded-id", name: "A.TXT"}, {id: "exact-id", name: "a.txt"}, {id: "other-id", name: "b.txt"}},
			wantID:   "exact-id", wantPages: namePage,
		},
		{
			label:    "only another spelling",
			children: []namedChild{{id: "folded-id", name: "A.TXT"}, {id: "other-id", name: "b.txt"}, {id: "more-id", name: "c.txt"}},
			wantID:   "folded-id", wantPages: everyPage,
		},
	} {
		t.Run(tc.label, func(t *testing.T) {
			t.Parallel()

			fake := &namedFolders{
				t:          t,
				children:   map[string][]namedChild{lookupParent: tc.children},
				readMisses: map[string]bool{lookupParent: true},
			}
			client := newTestSession(t, http.HandlerFunc(fake.serve)).Meta

			item, found, err := FindExistingChild(t.Context(), client, driveid.New("abcdef0123456789"), lookupParent, "a.txt")
			require.NoError(t, err)
			require.True(t, found)
			assert.Equal(t, tc.wantID, item.ID)
			assert.Equal(t, tc.wantPages, fake.pages.Load())
		})
	}
}

// A folder that holds an upload's name is not the upload's file, whether the
// read of the name or the folder's listing finds it.
//
// Validates: R-5.5.2
func TestLookupUploadTarget_AFolderOfTheNameIsNotTheUpload(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		label      string
		readMisses bool
	}{
		{label: "the read answers the folder"},
		{label: "the listing holds the folder", readMisses: true},
	} {
		t.Run(tc.label, func(t *testing.T) {
			t.Parallel()

			fake := &namedFolders{
				t:          t,
				children:   map[string][]namedChild{lookupParent: {{id: "folder-id", name: "a.txt", isFolder: true}}},
				readMisses: map[string]bool{lookupParent: tc.readMisses},
			}
			client := newTestSession(t, http.HandlerFunc(fake.serve)).Meta

			item, err := newTestTM(&tmSimpleDownloader{}, client, nil).lookupUploadTarget(t.Context(),
				uploadTarget{driveID: driveid.New("abcdef0123456789"), parentID: lookupParent, name: "a.txt"})
			require.ErrorIs(t, err, graph.ErrNotFound)
			assert.Nil(t, item)
		})
	}
}

// A path below a mounted folder that holds empty names ("a//b", or slashes at
// its ends) is the path without them, as everywhere: OneDrive holds no empty
// name. Each name is still read once, and no folder is listed.
//
// Validates: R-1.5.2, R-1.3.6
func TestMountSession_APathWithEmptyNamesIsThePathWithoutThem(t *testing.T) {
	t.Parallel()

	const (
		mountRoot = "mount-root-item"
		// namesOnPath is the names each path holds: Documents and report.docx.
		namesOnPath = 2
	)

	for _, path := range []string{"Documents//report.docx", "/Documents///report.docx/", "//Documents/report.docx"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			fake := &namedFolders{
				t: t,
				children: map[string][]namedChild{
					mountRoot:   {{id: "folder-id", name: "Documents", isFolder: true}},
					"folder-id": {{id: "file-id", name: "report.docx"}},
				},
			}
			session := NewMountSession(newTestSession(t, http.HandlerFunc(fake.serve)), mountRoot)

			lookups := []func(context.Context, string) (*graph.Item, error){
				session.ResolveItem, session.WaitPathVisible, session.ResolveDeleteTarget,
			}
			for _, resolve := range lookups {
				item, err := resolve(t.Context(), path)
				require.NoError(t, err)
				assert.Equal(t, "file-id", item.ID)
			}
			assert.Equal(t, int32(len(lookups)*namesOnPath), fake.reads.Load(), "each name read once by each lookup")
			assert.Zero(t, fake.pages.Load())
		})
	}
}

// laggingFilePages is how many listing pages laggingFileDrive's folder Docs
// serves, one child on each; the file is on laggingFilePage.
const (
	laggingFilePages = 3
	laggingFilePage  = 2
)

// laggingFileDrive is a Graph fake of a drive whose path route does not show
// Docs/file.txt yet: its read by path answers 404, and so does the listing of
// Docs by path when listingLags, which leaves the folder to be read by path
// and listed by ID. Either listing serves laggingFilePages pages of one child
// each, the file on laggingFilePage. It counts the pages asked for.
type laggingFileDrive struct {
	t           *testing.T
	listingLags bool
	pages       atomic.Int32
}

func (f *laggingFileDrive) serve(w http.ResponseWriter, r *http.Request) {
	const drive = "/drives/abcdef0123456789"

	switch r.URL.Path {
	case drive + "/root:/Docs/file.txt:":
		w.WriteHeader(http.StatusNotFound)
		writeTestResponsef(f.t, w, `{"error":{"code":"itemNotFound"}}`)
	case drive + "/root:/Docs:":
		writeTestResponsef(f.t, w, `{"id":"docs-id","name":"Docs","folder":{},"parentReference":{"id":"root-id","path":"/drive/root:"}}`)
	case drive + "/root:/Docs:/children":
		if f.listingLags {
			w.WriteHeader(http.StatusNotFound)
			writeTestResponsef(f.t, w, `{"error":{"code":"itemNotFound"}}`)
			return
		}
		f.page(w, r)
	case drive + "/items/docs-id/children":
		f.page(w, r)
	default:
		assert.Fail(f.t, "unexpected request", r.URL.String())
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}
}

func (f *laggingFileDrive) page(w http.ResponseWriter, r *http.Request) {
	page := 1
	if raw := r.URL.Query().Get("page"); raw != "" {
		var err error
		page, err = strconv.Atoi(raw)
		assert.NoError(f.t, err)
	}
	f.pages.Add(1)

	child := fmt.Sprintf(`{"id":"other-%d","name":"other-%d.txt","file":{}`, page, page)
	if page == laggingFilePage {
		child = `{"id":"file-id","name":"file.txt","file":{}`
	}
	child += `,"parentReference":{"id":"docs-id","path":"/drive/root:/Docs"}}`
	next := ""
	if page < laggingFilePages {
		next = fmt.Sprintf(`,"@odata.nextLink":"http://%s%s?page=%d"`, r.Host, r.URL.Path, page+1)
	}
	writeTestResponsef(f.t, w, `{"value":[%s]%s}`, child, next)
}

// fallbackLookups are the lookups that fall back to the listing of a path's
// parent when the read by path finds nothing: a delete's target and a
// visibility wait.
func fallbackLookups() map[string]func(*MountSession, context.Context, string) (*graph.Item, error) {
	return map[string]func(*MountSession, context.Context, string) (*graph.Item, error){
		"delete target":   (*MountSession).ResolveDeleteTarget,
		"visibility wait": (*MountSession).WaitPathVisible,
	}
}

// A path the read by path does not find yet is looked for in its parent's
// listing, by path or, when the parent's path lags too, by ID, and the listing
// stops at the page that holds the name.
//
// Validates: R-6.7.14
func TestMountSession_ParentListingStopsAtThePageThatHoldsTheName(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		label       string
		listingLags bool
	}{
		{label: "listed by path"},
		{label: "listed by ID", listingLags: true},
	} {
		t.Run(tc.label, func(t *testing.T) {
			t.Parallel()

			for lookup, resolve := range fallbackLookups() {
				fake := &laggingFileDrive{t: t, listingLags: tc.listingLags}
				session := NewMountSession(newTestSession(t, http.HandlerFunc(fake.serve)), "")

				item, err := resolve(session, t.Context(), "Docs/file.txt")
				require.NoError(t, err, lookup)
				assert.Equal(t, "file-id", item.ID, lookup)
				assert.Equal(t, int32(laggingFilePage), fake.pages.Load(), lookup)
			}
		})
	}
}

// A fallback takes no item from a listing whose answers place its children in
// another folder than the one at the path's parent. The fake's path route
// skips the folder v1.0, as a path query that answers from another path
// would, and its answers carry their parent paths, as a Personal drive's
// reads by path do: the read of Docs/v1.0/file.txt answers Docs/file.txt, and
// the listing of Docs/v1.0 by path Docs's children. Neither is the item at the
// path, so a delete or a visibility wait finds the folder v1.0 in Docs's
// listing and the file in v1.0's listing by ID.
//
// Validates: R-6.7.22, R-6.7.14
func TestMountSession_AFallbackTakesNoItemFromAListingThatNamesAnotherFolder(t *testing.T) {
	t.Parallel()

	const (
		drive       = "/drives/abcdef0123456789"
		docsFile    = `{"id":"wrong-id","name":"file.txt","file":{},"parentReference":{"id":"docs-id","path":"/drive/root:/Docs"}}`
		docsVersion = `{"id":"v1-id","name":"v1.0","folder":{},"parentReference":{"id":"docs-id","path":"/drive/root:/Docs"}}`
	)

	for lookup, resolve := range fallbackLookups() {
		t.Run(lookup, func(t *testing.T) {
			t.Parallel()

			session := NewMountSession(newTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case drive + "/root:/Docs/v1.0/file.txt:":
					writeTestResponsef(t, w, "%s", docsFile)
				case drive + "/root:/Docs/v1.0:":
					writeTestResponsef(t, w, `{"id":"docs-id","name":"Docs","folder":{},"parentReference":{"id":"root-id","path":"/drive/root:"}}`)
				case drive + "/root:/Docs/v1.0:/children", drive + "/root:/Docs:/children":
					writeTestResponsef(t, w, `{"value":[%s,%s]}`, docsFile, docsVersion)
				case drive + "/items/v1-id/children":
					writeTestResponsef(t, w, `{"value":[{"id":"right-id","name":"file.txt","file":{},`+
						`"parentReference":{"id":"v1-id","path":"/drive/root:/Docs/v1.0"}}]}`)
				default:
					assert.Fail(t, "unexpected request", r.URL.String())
					http.Error(w, "unexpected request", http.StatusInternalServerError)
				}
			})), "")

			item, err := resolve(session, t.Context(), "Docs/v1.0/file.txt")
			require.NoError(t, err)
			assert.Equal(t, "right-id", item.ID)
		})
	}
}

// disagreeingDrive is a Graph fake of a drive whose answers carry no parent
// path, with a/b/c/f.txt where the path route and the listing route name
// different folders for one path. Its folder c (c-id) holds right-f; the path
// route reads a/b/c as x-c, a folder c in a folder b (x-b) in a folder x, the
// declared gap of a read held by its parent alone, and x-c holds wrong-f.
// settle makes the answers agree on c (settledAnswers). GETs not in the
// answers answer 404, and every other request fails the test. It records each
// request as its method and path.
type disagreeingDrive struct {
	t        *testing.T
	mu       sync.Mutex
	answers  map[string]string
	requests []string
}

// disagreeingDriveRoute is the drive part of every route disagreeingDrive
// answers.
const disagreeingDriveRoute = "/drives/abcdef0123456789"

const (
	disagreeingRightFile = `{"id":"right-f","name":"f.txt","file":{},"parentReference":{"id":"c-id"}}`
	disagreeingWrongFile = `{"id":"wrong-f","name":"f.txt","file":{},"parentReference":{"id":"x-c"}}`
)

// disagreeingShapes are the two ways the routes disagree. In "the listing
// and the read of the folder" the file's read lags (404) and the listing of
// a/b/c lists c, while the read of a/b/c answers x-c. In "the read and the
// read of its folder" the file's read answers right-f in c, while the read of
// a/b/c answers x-c and the listing of a/b/c lists x-c.
func disagreeingShapes() map[string]map[string]string {
	const (
		otherFolderC = `{"id":"x-c","name":"c","folder":{},"parentReference":{"id":"x-b"}}`
		otherFolderB = `{"id":"x-b","name":"b","folder":{},"parentReference":{"id":"x"}}`
	)
	root := disagreeingDriveRoute + "/root:/"
	otherListing := disagreeingDriveRoute + "/items/x-c/children"

	return map[string]map[string]string{
		readDisagreesWithTheListing: {
			root + "a/b/c:/children": `{"value":[` + disagreeingRightFile + `]}`,
			root + "a/b/c:":          otherFolderC,
			root + "a/b:":            otherFolderB,
			otherListing:             `{"value":[` + disagreeingWrongFile + `]}`,
		},
		readDisagreesWithTheFolderRead: {
			root + "a/b/c/f.txt:":    disagreeingRightFile,
			root + "a/b/c:":          otherFolderC,
			root + "a/b:":            otherFolderB,
			root + "a/b/c:/children": `{"value":[` + disagreeingWrongFile + `]}`,
			otherListing:             `{"value":[` + disagreeingWrongFile + `]}`,
		},
	}
}

// The names of the two disagreeingShapes.
const (
	readDisagreesWithTheListing    = "the listing and the read of the folder"
	readDisagreesWithTheFolderRead = "the read and the read of its folder"
)

// settledAnswers are disagreeingDrive's answers once they agree: the file is
// right-f in c, and a/b/c is c.
func settledAnswers() map[string]string {
	root := disagreeingDriveRoute + "/root:/"

	return map[string]string{
		root + "a/b/c/f.txt:": disagreeingRightFile,
		root + "a/b/c:":       `{"id":"c-id","name":"c","folder":{},"parentReference":{"id":"b-id"}}`,
	}
}

func (f *disagreeingDrive) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	answer, ok := f.answers[r.URL.Path]
	f.mu.Unlock()

	if ok && r.Method == http.MethodGet {
		writeTestResponsef(f.t, w, "%s", answer)
		return
	}
	if r.Method == http.MethodGet ||
		(r.Method == http.MethodDelete && r.URL.Path == disagreeingDriveRoute+"/items/right-f") ||
		(r.Method == http.MethodPost && r.URL.Path == disagreeingDriveRoute+"/items/right-f/permanentDelete") {
		// The first delete of the right file lags as a delete can, so a
		// delete looks the path up again.
		w.WriteHeader(http.StatusNotFound)
		writeTestResponsef(f.t, w, `{"error":{"code":"itemNotFound"}}`)
		return
	}

	assert.Fail(f.t, "unexpected request", r.Method+" "+r.URL.String())
	w.WriteHeader(http.StatusForbidden)
}

// settle makes the drive's answers agree from its next request on.
func (f *disagreeingDrive) settle() {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.answers = settledAnswers()
}

// sent is every request the drive answered, in order.
func (f *disagreeingDrive) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.requests)
}

// otherFolderListed reports whether the drive was asked for the listing of
// x-c, the folder only one of its answers names.
func (f *disagreeingDrive) otherFolderListed() bool {
	return slices.Contains(f.sent(), http.MethodGet+" "+disagreeingDriveRoute+"/items/x-c/children")
}

// anyListing reports whether the drive was asked for any folder's children.
func (f *disagreeingDrive) anyListing() bool {
	return slices.ContainsFunc(f.sent(), func(request string) bool { return strings.HasSuffix(request, "/children") })
}

// disagreeingSession is a drive-root session on fake whose visibility wait
// sleeps twice, through onSleep.
func disagreeingSession(t *testing.T, fake *disagreeingDrive, onSleep func()) *MountSession {
	t.Helper()

	session := NewMountSession(newTestSession(t, http.HandlerFunc(fake.serve)), "")
	session.visibilityWaitSchedule = []time.Duration{time.Nanosecond, time.Nanosecond}
	session.sleepFunc = func(context.Context, time.Duration) error {
		onSleep()
		return nil
	}

	return session
}

// requireAnswersDisagree asserts that err reports answers that name different
// folders for one path, and is no "not found", which a caller would take as
// leave to look elsewhere.
func requireAnswersDisagree(t *testing.T, err error, msgAndArgs ...any) {
	t.Helper()

	require.ErrorIs(t, err, graph.ErrPathAnswersDisagree, msgAndArgs...)
	require.NotErrorIs(t, err, graph.ErrNotFound, msgAndArgs...)
}

// When the answers for a path name different folders for it, a delete's
// target is refused at once with ErrPathAnswersDisagree, which is not "not
// found", before either folder is listed by ID or an item taken from it: the
// answers do not show which folder is at the path, and taking the item from
// the folder one of them names would act on another folder's file.
//
// Validates: R-6.7.22, R-6.7.14
func TestMountSession_ADeleteTargetIsNotTakenWhereAnswersNameDifferentFolders(t *testing.T) {
	t.Parallel()

	for shape, answers := range disagreeingShapes() {
		t.Run(shape, func(t *testing.T) {
			t.Parallel()

			fake := &disagreeingDrive{t: t, answers: answers}
			var sleeps int
			session := disagreeingSession(t, fake, func() { sleeps++ })

			item, err := session.ResolveDeleteTarget(t.Context(), "a/b/c/f.txt")
			requireAnswersDisagree(t, err)
			assert.Nil(t, item)
			assert.Zero(t, sleeps, "a delete's target is not waited for")
			assert.False(t, fake.otherFolderListed(), "the folder one answer names is not listed")
		})
	}
}

// A visibility wait whose answers name different folders for the path reads
// the path again, as it does when the path is not found yet: such answers can
// settle once OneDrive converges after a change. It takes no item from a
// listing because of them, and answers that still disagree when the wait runs
// out are a path not visible yet, as one never found is, with the
// disagreement as its cause.
//
// Validates: R-6.7.22, R-6.7.14
func TestMountSession_AVisibilityWaitReadsAgainWhileAnswersNameDifferentFolders(t *testing.T) {
	t.Parallel()

	for shape, answers := range disagreeingShapes() {
		t.Run(shape+"/settling", func(t *testing.T) {
			t.Parallel()

			fake := &disagreeingDrive{t: t, answers: answers}
			session := disagreeingSession(t, fake, fake.settle)

			item, err := session.WaitPathVisible(t.Context(), "a/b/c/f.txt")
			require.NoError(t, err)
			assert.Equal(t, "right-f", item.ID)
			assert.False(t, fake.otherFolderListed(), "the folder one answer names is not listed")
			if shape == readDisagreesWithTheFolderRead {
				assert.False(t, fake.anyListing(), "no folder is listed because of answers that disagree")
			}
		})

		t.Run(shape+"/still disagreeing", func(t *testing.T) {
			t.Parallel()

			fake := &disagreeingDrive{t: t, answers: answers}
			var sleeps int
			session := disagreeingSession(t, fake, func() { sleeps++ })

			item, err := session.WaitPathVisible(t.Context(), "a/b/c/f.txt")
			var notVisible *PathNotVisibleError
			require.ErrorAs(t, err, &notVisible)
			assert.Equal(t, "a/b/c/f.txt", notVisible.Path)
			require.ErrorIs(t, err, graph.ErrPathAnswersDisagree, "the disagreement is kept as the cause")
			assert.Nil(t, item)
			assert.Equal(t, 2, sleeps, "the path is read once more after each wait")
			assert.False(t, fake.otherFolderListed(), "the folder one answer names is not listed")
			if shape == readDisagreesWithTheFolderRead {
				assert.False(t, fake.anyListing(), "no folder is listed because of answers that disagree")
			}
		})
	}
}

// A delete whose first request finds nothing looks the path up again before
// it deletes anything else, and stops when the answers for the path name
// different folders for it: rm's retry and the --force cleanup of mv and cp
// delete through it.
//
// Validates: R-6.7.22, R-1.4.4
func TestMountSession_DeleteResolvedPathStopsWhereAnswersNameDifferentFolders(t *testing.T) {
	t.Parallel()

	deletes := map[string]func(*MountSession, context.Context, string, string) error{
		"recycle bin": (*MountSession).DeleteResolvedPath,
		"permanent":   (*MountSession).PermanentDeleteResolvedPath,
	}
	for shape, answers := range disagreeingShapes() {
		for mode, deletePath := range deletes {
			t.Run(shape+"/"+mode, func(t *testing.T) {
				t.Parallel()

				fake := &disagreeingDrive{t: t, answers: answers}
				session := NewMountSession(newTestSession(t, http.HandlerFunc(fake.serve)), "")
				session.visibilityWaitSchedule = []time.Duration{0}

				err := deletePath(session, t.Context(), "a/b/c/f.txt", "right-f")
				requireAnswersDisagree(t, err)
				for _, request := range fake.sent() {
					assert.NotContains(t, request, "wrong-f", "no request names the other folder's file")
				}
			})
		}
	}
}

// When the answers agree that the item a read by path answered is elsewhere
// (the folder it sits in is answered at another path, or under another
// name), the read finds nothing at the path, and a delete's target is looked
// for in the parent's listing as for any path not found: the listing of the
// parent by path holds the same misplaced item and is refused the same way,
// so the parent is found in its own parent's listing and listed by ID.
//
// Validates: R-6.7.22, R-6.7.14
func TestMountSession_AReadTheAnswersPutElsewhereFallsBackToTheListing(t *testing.T) {
	t.Parallel()

	root := disagreeingDriveRoute + "/root:/"
	const misplaced = `{"id":"a-other","name":"a.txt","file":{},"parentReference":{"id":"sub-other"}}`
	for label, folder := range map[string]string{
		"its folder at another path":    `{"id":"sub-other","name":"Sub","folder":{},"parentReference":{"id":"other-id","path":"/drive/root:/Other"}}`,
		"its folder under another name": `{"id":"sub-other","name":"Other","folder":{},"parentReference":{"id":"other-id"}}`,
	} {
		t.Run(label, func(t *testing.T) {
			t.Parallel()

			fake := &disagreeingDrive{t: t, answers: map[string]string{
				root + "Docs/Sub/a.txt:":    misplaced,
				root + "Docs/Sub:":          folder,
				root + "Docs/Sub:/children": `{"value":[` + misplaced + `]}`,
				root + "Docs:":              `{"id":"docs-id","name":"Docs","folder":{},"parentReference":{"id":"root-id","path":"/drive/root:"}}`,
				root + "Docs:/children": `{"value":[{"id":"sub-id","name":"Sub","folder":{},` +
					`"parentReference":{"id":"docs-id","path":"/drive/root:/Docs"}}]}`,
				disagreeingDriveRoute + "/items/sub-id/children": `{"value":[{"id":"right-a","name":"a.txt","file":{},` +
					`"parentReference":{"id":"sub-id"}}]}`,
			}}
			session := NewMountSession(newTestSession(t, http.HandlerFunc(fake.serve)), "")

			item, err := session.ResolveDeleteTarget(t.Context(), "Docs/Sub/a.txt")
			require.NoError(t, err)
			assert.Equal(t, "right-a", item.ID)
		})
	}
}
