package cli

import (
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	stdsync "sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// disagreeingDriveRoute is the drive part of every route disagreeingDrive
// answers.
const disagreeingDriveRoute = "/drives/0000000" + rootTestDrive123

// disagreeingDrive is a Graph fake of a drive whose answers carry no parent
// path, where the path route and the listing route name different folders
// for the path a/b/c. Its folder c (c-id) holds the file right-f (f.txt) and
// the folder right-d (d); the path route reads a/b/c as x-c, a folder c in a
// folder b (x-b) in a folder x, which a read held by its parent alone takes
// for a/b/c, and x-c holds wrong-f and wrong-d of the same names. GETs not
// in answers answer 404; a DELETE of a path in deletable answers 204, and
// every other request is refused (403); both are recorded, so a test sees
// whether a command tried to change anything.
type disagreeingDrive struct {
	t         *testing.T
	answers   map[string]string
	deletable map[string]bool

	mu        stdsync.Mutex
	mutations []string
}

// disagreeingShapes are the two ways the routes disagree. In "the listing
// and the read of the folder" reads below a/b/c lag (404) and the listing of
// a/b/c lists c, while the read of a/b/c answers x-c. In "the read and the
// read of its folder" the reads of a/b/c/f.txt and a/b/c/d answer the items
// in c, while the read and the listing of a/b/c answer x-c.
func disagreeingShapes() map[string]map[string]string {
	const (
		otherFolderC = `{"id":"x-c","name":"c","folder":{},"parentReference":{"id":"x-b"}}`
		otherFolderB = `{"id":"x-b","name":"b","folder":{},"parentReference":{"id":"x"}}`
		rightFile    = `{"id":"right-f","name":"f.txt","file":{},"parentReference":{"id":"c-id"}}`
		rightFolder  = `{"id":"right-d","name":"d","folder":{},"parentReference":{"id":"c-id"}}`
		wrongItems   = `{"value":[{"id":"wrong-f","name":"f.txt","file":{},"parentReference":{"id":"x-c"}},` +
			`{"id":"wrong-d","name":"d","folder":{},"parentReference":{"id":"x-c"}}]}`
	)
	root := disagreeingDriveRoute + "/root:/"
	otherListing := disagreeingDriveRoute + "/items/x-c/children"

	return map[string]map[string]string{
		"the listing and the read of the folder": {
			root + "a/b/c:/children": `{"value":[` + rightFile + `,` + rightFolder + `]}`,
			root + "a/b/c:":          otherFolderC,
			root + "a/b:":            otherFolderB,
			otherListing:             wrongItems,
		},
		"the read and the read of its folder": {
			root + "a/b/c/f.txt:":    rightFile,
			root + "a/b/c/d:":        rightFolder,
			root + "a/b/c:":          otherFolderC,
			root + "a/b:":            otherFolderB,
			root + "a/b/c:/children": wrongItems,
			otherListing:             wrongItems,
		},
	}
}

func (f *disagreeingDrive) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodGet {
		f.mu.Lock()
		f.mutations = append(f.mutations, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		if r.Method == http.MethodDelete && f.deletable[r.URL.Path] {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusForbidden)
		writeTestResponse(f.t, w, `{"error":{"code":"accessDenied"}}`)

		return
	}

	answer, ok := f.answers[r.URL.Path]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		writeTestResponse(f.t, w, `{"error":{"code":"itemNotFound"}}`)

		return
	}
	writeTestResponse(f.t, w, answer)
}

// tried is every request the drive got that would change something.
func (f *disagreeingDrive) tried() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.mutations)
}

// disagreeingDriveContext is a data-command context backed by fake, whose
// visibility waits read a path twice without sleeping.
func disagreeingDriveContext(t *testing.T, fake *disagreeingDrive) *CLIContext {
	t.Helper()

	cid := driveid.MustCanonicalID(jsonContractDrive)
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	require.NoError(t, os.MkdirAll(filepath.Dir(config.DriveStatePath(cid)), 0o700))

	logger := slog.New(slog.DiscardHandler)
	runtime := driveops.NewSessionRuntime(nil, "test-agent", logger)
	runtime.GraphBaseURL = srv.URL
	runtime.VisibilityWaitSchedule = []time.Duration{0}
	runtime.TokenSourceFn = func(string, *slog.Logger) (graph.TokenSource, error) {
		return staticTokenSource{}, nil
	}

	return &CLIContext{
		Logger:  logger,
		Cfg:     &config.ResolvedDrive{CanonicalID: cid, DriveID: driveid.New(rootTestDrive123)},
		Runtime: runtime,
	}
}

// requireNothingTriedWhereAnswersDisagree asserts that a command failed on
// answers that name different folders for one path, without calling it "not
// found", and sent no request that changes anything.
func requireNothingTriedWhereAnswersDisagree(t *testing.T, fake *disagreeingDrive, run folderCommandRun) {
	t.Helper()

	require.ErrorIs(t, run.err, graph.ErrPathAnswersDisagree)
	require.NotErrorIs(t, run.err, graph.ErrNotFound)
	assert.Empty(t, fake.tried(), "no request may change the drive")
}

// rm deletes nothing when the answers for its path name different folders
// for it: the file or folder it would take is either the one at the path or
// another folder's item of the same name, and the answers do not show which.
//
// Validates: R-1.4.3, R-6.7.22
func TestRunRm_DeletesNothingWhereAnswersNameDifferentFolders(t *testing.T) {
	for shape, answers := range disagreeingShapes() {
		for _, target := range []struct{ path, flag string }{
			{path: "/a/b/c/f.txt"},
			{path: "/a/b/c/d", flag: "--recursive"},
		} {
			t.Run(shape+target.path, func(t *testing.T) {
				setTestDriveHome(t)
				fake := &disagreeingDrive{t: t, answers: answers}
				cc := disagreeingDriveContext(t, fake)

				args := []string{target.path}
				if target.flag != "" {
					args = append([]string{target.flag}, args...)
				}
				requireNothingTriedWhereAnswersDisagree(t, fake, runFileCommand(t, "onedrive-go rm", cc, false, args...))
			})
		}
	}
}

// When rm's delete has landed and the answers for the parent folder still
// name different folders for it, rm succeeds with the note it gives a parent
// that is not visible yet: the wait reads the parent again, as such answers
// can settle, and runs out as for a parent not found.
//
// Validates: R-1.4.4, R-6.7.22
func TestRunRm_NotesAParentWhoseAnswersStillDisagreeAfterTheDelete(t *testing.T) {
	setTestDriveHome(t)
	root := disagreeingDriveRoute + "/root:/"
	fake := &disagreeingDrive{
		t: t,
		answers: map[string]string{
			root + "a/b/c/f.txt:": `{"id":"right-f","name":"f.txt","file":{},"parentReference":{"id":"c-id"}}`,
			root + "a/b/c:":       `{"id":"c-id","name":"c","folder":{},"parentReference":{"id":"b-id"}}`,
			root + "a/b:":         `{"id":"x-b","name":"b","folder":{},"parentReference":{"id":"x"}}`,
		},
		deletable: map[string]bool{disagreeingDriveRoute + "/items/right-f": true},
	}
	cc := disagreeingDriveContext(t, fake)

	run := runFileCommand(t, "onedrive-go rm", cc, false, "/a/b/c/f.txt")
	require.NoError(t, run.err)
	assert.Equal(t, []string{http.MethodDelete + " " + disagreeingDriveRoute + "/items/right-f"}, fake.tried())
	assert.Contains(t, run.stderr, "Note: deleted /a/b/c/f.txt; OneDrive may take a moment to show the change in /a/b/c.")
}

// put uploads nothing when the answers for its remote parent name different
// folders for it: the folder it would upload into, replacing a file of the
// same name, is either the one at the path or another folder of the same
// name, and the answers do not show which. Its wait for the parent reads it
// again, as such answers can settle, and ends as for a parent not visible,
// with the disagreement as its cause.
//
// Validates: R-1.3.8, R-6.7.22
func TestRunPut_UploadsNothingWhereAnswersNameDifferentFolders(t *testing.T) {
	local := t.TempDir()
	writeEmptyLocalFiles(t, local, "f.txt", "up/f.txt")

	for shape, answers := range disagreeingShapes() {
		for _, source := range []string{"f.txt", "up"} {
			t.Run(shape+"/"+source, func(t *testing.T) {
				setTestDriveHome(t)
				fake := &disagreeingDrive{t: t, answers: answers}
				cc := disagreeingDriveContext(t, fake)

				run := runFileCommand(t, putCommand, cc, false, filepath.Join(local, source), "/a/b/c/d/"+source)
				var notVisible *driveops.PathNotVisibleError
				require.ErrorAs(t, run.err, &notVisible)
				require.ErrorIs(t, run.err, graph.ErrPathAnswersDisagree, "the disagreement is kept as the cause")
				assert.Empty(t, fake.tried(), "no request may change the drive")
				assert.NotContains(t, run.stderr, "Uploaded")
			})
		}
	}
}

// requireOneSentenceWithoutItemIDs asserts that the error of a command whose
// answers for path disagree prints one sentence that names path once and no
// item ID, and that --verbose's details keep the chain with the IDs.
func requireOneSentenceWithoutItemIDs(t *testing.T, err error, path string) {
	t.Helper()

	plain := commandErrorText(err, false)
	assert.Equal(t, "Error: OneDrive's answers for "+path+" do not agree on which folder holds it. "+
		"This usually settles within a few minutes: wait, then check it and run the command again.", plain)
	for _, id := range []string{"x-c", "x-b", "c-id", "right-f", "right-d"} {
		assert.NotContains(t, plain, id, "no item ID in the sentence")
	}

	verbose := commandErrorText(err, true)
	_, details, found := strings.Cut(verbose, "\nDetails: ")
	require.True(t, found, verbose)
	assert.Contains(t, details, "x-c", "--verbose keeps the chain, item IDs included")
	assert.Equal(t, 1, strings.Count(verbose, "Error: "), "the error is printed once")
}

// A command whose path's answers disagree says so in one sentence of its own
// that names the path once and no item ID, rm before it deletes anything and
// put at the end of its wait for its folder alike; --verbose's details keep
// the chain. The sentence does not say that nothing changed: a confirmation
// wait meets the same disagreement after its change landed.
//
// Validates: R-6.6.26, R-6.7.22
func TestCommandErrorText_DisagreeingAnswersForAPathAreOneSentence(t *testing.T) {
	for shape, answers := range disagreeingShapes() {
		t.Run(shape+"/rm", func(t *testing.T) {
			setTestDriveHome(t)
			fake := &disagreeingDrive{t: t, answers: answers}
			cc := disagreeingDriveContext(t, fake)

			run := runFileCommand(t, "onedrive-go rm", cc, false, "/a/b/c/f.txt")

			require.ErrorIs(t, run.err, graph.ErrPathAnswersDisagree)
			requireOneSentenceWithoutItemIDs(t, run.err, "/a/b/c/f.txt")
		})

		t.Run(shape+"/put", func(t *testing.T) {
			setTestDriveHome(t)
			local := t.TempDir()
			writeEmptyLocalFiles(t, local, "f.txt")
			fake := &disagreeingDrive{t: t, answers: answers}
			cc := disagreeingDriveContext(t, fake)

			run := runFileCommand(t, putCommand, cc, false, filepath.Join(local, "f.txt"), "/a/b/c/d/f.txt")

			var notVisible *driveops.PathNotVisibleError
			require.ErrorAs(t, run.err, &notVisible)
			require.ErrorIs(t, run.err, graph.ErrPathAnswersDisagree)
			requireOneSentenceWithoutItemIDs(t, run.err, disagreeingSubject(run.err))
		})
	}
}

// disagreeingSubject is the path the sentence for err names: the path the
// command recorded, or the path its visibility wait ran out on.
func disagreeingSubject(err error) string {
	var op *remoteOpError
	if errors.As(err, &op) {
		return op.path
	}
	var notVisible *driveops.PathNotVisibleError
	if errors.As(err, &notVisible) {
		return notVisible.Path
	}

	return "the path"
}

// A name with ':' is read through its parent folder's listing, never by its
// path, so a missing one is one sentence that names the path once, with the
// folder to list; --verbose keeps the chain.
//
// Validates: R-6.6.26, R-6.7.38
func TestCommandErrorText_ANameAPathCannotCarryIsOneSentence(t *testing.T) {
	setTestDriveHome(t)
	fake := &disagreeingDrive{t: t, answers: map[string]string{}}
	cc := disagreeingDriveContext(t, fake)

	run := runFileCommand(t, "onedrive-go stat", cc, false, "/a/b:c.txt")

	require.ErrorIs(t, run.err, graph.ErrNotFound)
	require.NotErrorIs(t, run.err, graph.ErrNameNotPathAddressable, "the name is looked up in its folder, not refused")
	assert.Equal(t, "Error: /a/b:c.txt was not found on OneDrive. "+
		"Run 'onedrive-go ls /a' to see what is there.", commandErrorText(run.err, false))
	assert.Contains(t, commandErrorText(run.err, true), "\nDetails: resolving \"/a/b:c.txt\": ")
}
