package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// A made-up business account with two made-up SharePoint sites: one that
// lists its library and one whose library listing can be refused.
const (
	siteListingTestTokenFile     = "token_business_user@example.com.json"
	siteListingTestAccount       = "user@example.com"
	siteListingTestBusinessDrive = "business:user@example.com"
	siteListingTestHarborSite    = "Harbor Team"
	siteListingTestLanternSite   = "Lantern Team"
	siteListingTestHarborLibrary = "sharepoint:user@example.com:harbor:Documents"
	siteListingTestHarborDrives  = "/sites/site-harbor/drives"
	siteListingTestLanternDrives = "/sites/site-lantern/drives"
	siteListingTestQuery         = "team"
	siteListingTestRefusal       = "access denied"
	siteListingTestIncomplete    = "Note: this listing is incomplete.\n"
	siteListingTestLanternNote   = "Note: could not list the libraries of SharePoint site Lantern Team of user@example.com: access denied.\n"
	siteListingTestHarborNote    = "Note: could not list the libraries of SharePoint site Harbor Team of user@example.com: access denied.\n"

	// A made-up request id and a made-up page that an unrecognized refusal
	// carries; neither may reach a note or a document.
	siteListingTestRequestID = "made-up-request-0415"
	siteListingTestPageText  = "made-up refusal page"

	// A second made-up business account, told apart by its saved sign-in.
	siteListingTestOtherAccount    = "other@example.com"
	siteListingTestOtherSignIn     = "made-up-sign-in-other"
	siteListingTestOtherSearchNote = "Note: could not search the SharePoint sites of other@example.com: access denied.\n"
)

// siteListingFake says which of the fake's SharePoint answers are refusals,
// and how the fake answers.
type siteListingFake struct {
	refuseSiteSearch bool
	refuseHarbor     bool
	refuseLantern    bool
	// unrecognizedRefusal refuses with 415, a request id and a page of two
	// lines instead of 403 accessDenied: a status with no sentinel, whose
	// error text holds the id and the page.
	unrecognizedRefusal bool
	// sitesReversed lists Lantern before Harbor in the site search.
	sitesReversed bool
	// harborWithoutDisplayName answers the site search with Harbor's URL name
	// alone.
	harborWithoutDisplayName bool
	// stopAtHarbor, when set, is called by Harbor's library listing, which
	// then answers nothing until the request ends: a stop that lands while a
	// site is being listed.
	stopAtHarbor context.CancelFunc
	// stopAtSiteSearch does the same in the site search: a stop that lands
	// before any site is listed.
	stopAtSiteSearch context.CancelFunc
}

// newSiteListingGraph serves the account's drives, its site search and each
// site's libraries, refusing the ones fake names with 403 accessDenied (a
// status the retrying transport does not retry) or, with
// unrecognizedRefusal, with 415.
func newSiteListingGraph(t *testing.T, fake siteListingFake) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(siteListingHandler(t, fake))
	t.Cleanup(srv.Close)

	return srv
}

func siteListingHandler(t *testing.T, fake siteListingFake) http.HandlerFunc {
	t.Helper()

	refuse := func(w http.ResponseWriter) {
		if fake.unrecognizedRefusal {
			w.Header().Set("Content-Type", "text/html")
			w.Header().Set("request-id", siteListingTestRequestID)
			w.WriteHeader(http.StatusUnsupportedMediaType)
			writeTestResponse(t, w, "<html><title>Refused</title>\n<body>"+siteListingTestPageText+"</body></html>\n")
			return
		}
		w.WriteHeader(http.StatusForbidden)
		writeTestResponse(t, w, `{"error":{"code":"accessDenied","message":"Access denied"}}`)
	}

	harbor := `{"id":"site-harbor","displayName":"Harbor Team","name":"harbor","webUrl":"https://example.com/sites/harbor"}`
	if fake.harborWithoutDisplayName {
		harbor = `{"id":"site-harbor","name":"harbor","webUrl":"https://example.com/sites/harbor"}`
	}
	lantern := `{"id":"site-lantern","displayName":"Lantern Team","name":"lantern","webUrl":"https://example.com/sites/lantern"}`
	sites := harbor + "," + lantern
	if fake.sitesReversed {
		sites = lantern + "," + harbor
	}

	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Path {
		case graphDrivesPath:
			writeTestResponse(t, w, `{"value":[{"id":"drive-business","name":"OneDrive","driveType":"business"}]}`)
		case testGraphMePath:
			writeTestResponse(t, w, `{"id":"user-1","displayName":"User Example","mail":"user@example.com"}`)
		case testDriveSearchAllPath:
			writeTestResponse(t, w, `{"value":[]}`)
		case testGraphSitesPath:
			if fake.stopAtSiteSearch != nil {
				fake.stopAtSiteSearch()
				<-r.Context().Done()
				return
			}
			if fake.refuseSiteSearch {
				refuse(w)
				return
			}
			writeTestResponse(t, w, `{"value":[`+sites+`]}`)
		case siteListingTestHarborDrives:
			if fake.stopAtHarbor != nil {
				fake.stopAtHarbor()
				<-r.Context().Done()
				return
			}
			if fake.refuseHarbor {
				refuse(w)
				return
			}
			writeTestResponse(t, w, `{"value":[{"id":"library-harbor","name":"Documents","driveType":"documentLibrary"}]}`)
		case siteListingTestLanternDrives:
			if fake.refuseLantern {
				refuse(w)
				return
			}
			writeTestResponse(t, w, `{"value":[{"id":"library-lantern","name":"Reports","driveType":"documentLibrary"}]}`)
		default:
			assert.Fail(t, "unexpected graph path", "path=%s", r.URL.Path)
			http.Error(w, "unexpected graph path", http.StatusInternalServerError)
		}
	}
}

// siteListingRun is one run of drive list or drive search against the fake,
// with stdout and stderr apart.
type siteListingRun struct {
	stdout bytes.Buffer
	stderr bytes.Buffer
}

// startSiteListing signs in the made-up account and returns the command
// context of a run against graphURL.
func startSiteListing(t *testing.T, graphURL string, flags CLIFlags) (*siteListingRun, *CLIContext) {
	t.Helper()

	run := &siteListingRun{}
	cc := &CLIContext{
		Flags:        flags,
		Logger:       slog.New(slog.DiscardHandler),
		OutputWriter: &run.stdout,
		StatusWriter: &run.stderr,
		CfgPath:      filepath.Join(t.TempDir(), "missing-config.toml"),
		GraphBaseURL: graphURL,
	}

	return run, cc
}

func signInSiteListingAccount(t *testing.T) {
	t.Helper()

	setTestDriveHome(t)
	writeTestTokenFile(t, config.DefaultDataDir(), siteListingTestTokenFile)
}

func runSiteListingWithFlags(
	t *testing.T,
	fake siteListingFake,
	flags CLIFlags,
	command func(context.Context, *CLIContext) error,
) *siteListingRun {
	t.Helper()

	signInSiteListingAccount(t)
	run, cc := startSiteListing(t, newSiteListingGraph(t, fake).URL, flags)

	require.NoError(t, command(t.Context(), cc), "a listing that names what did not answer has not failed")

	return run
}

func runSiteListing(
	t *testing.T,
	fake siteListingFake,
	asJSON bool,
	command func(context.Context, *CLIContext) error,
) *siteListingRun {
	t.Helper()

	return runSiteListingWithFlags(t, fake, CLIFlags{JSON: asJSON}, command)
}

func runDriveListForSites(ctx context.Context, cc *CLIContext) error {
	return runDriveListWithContext(ctx, cc, false)
}

func runDriveSearchForSites(ctx context.Context, cc *CLIContext) error {
	return runDriveSearchWithContext(ctx, cc, siteListingTestQuery)
}

// siteListingCommand is one command in one form.
type siteListingCommand struct {
	name    string
	command func(context.Context, *CLIContext) error
	asJSON  bool
}

// siteListingCommands are both commands in both forms.
func siteListingCommands() []siteListingCommand {
	return []siteListingCommand{
		{"drive list text", runDriveListForSites, false},
		{"drive list json", runDriveListForSites, true},
		{"drive search text", runDriveSearchForSites, false},
		{"drive search json", runDriveSearchForSites, true},
	}
}

// siteListingDocument is the part of either command's --json document these
// tests read, decoded by field name so it holds the names scripts see.
type siteListingDocument struct {
	Available     []driveListEntry    `json:"available"`
	Results       []driveSearchResult `json:"results"`
	SitesUnlisted []struct {
		Account  string `json:"account"`
		SiteName string `json:"site_name"`
		Error    string `json:"error"`
	} `json:"sites_unlisted"`
	SiteSearchError *string `json:"site_search_error"`
}

func decodeSiteListing(t *testing.T, run *siteListingRun) (siteListingDocument, map[string]json.RawMessage) {
	t.Helper()

	var doc siteListingDocument
	require.NoError(t, json.Unmarshal(run.stdout.Bytes(), &doc), "stdout is one JSON document: %q", run.stdout.String())

	var keys map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(run.stdout.Bytes(), &keys))

	return doc, keys
}

func availableCanonicalIDs(entries []driveListEntry) []string {
	ids := make([]string, 0, len(entries))
	for i := range entries {
		ids = append(ids, entries[i].CanonicalID)
	}

	return ids
}

func unlistedSiteNames(doc *siteListingDocument) []string {
	names := make([]string, 0, len(doc.SitesUnlisted))
	for _, site := range doc.SitesUnlisted {
		names = append(names, site.SiteName)
	}

	return names
}

// A site whose libraries cannot be listed is named with its account, on
// stderr in both forms and in --json, with the listing said to be incomplete;
// the other site's library and the account's own drive are still listed, and
// the command exits 0.
//
// Validates: R-3.3.14
func TestDriveList_NamesASiteWhoseLibrariesCouldNotBeListed(t *testing.T) {
	fake := siteListingFake{refuseLantern: true}

	t.Run("text", func(t *testing.T) {
		run := runSiteListing(t, fake, false, runDriveListForSites)

		assert.Contains(t, run.stdout.String(), siteListingTestBusinessDrive)
		assert.Contains(t, run.stdout.String(), siteListingTestHarborLibrary+" ("+siteListingTestHarborSite+")")
		assert.NotContains(t, run.stdout.String(), siteListingTestLanternSite)
		assert.Equal(t, siteListingTestLanternNote+siteListingTestIncomplete, run.stderr.String())
	})

	t.Run("json", func(t *testing.T) {
		run := runSiteListing(t, fake, true, runDriveListForSites)

		doc, keys := decodeSiteListing(t, run)
		assert.Equal(t, []string{siteListingTestBusinessDrive, siteListingTestHarborLibrary}, availableCanonicalIDs(doc.Available))
		require.Len(t, doc.SitesUnlisted, 1)
		assert.Equal(t, siteListingTestAccount, doc.SitesUnlisted[0].Account)
		assert.Equal(t, siteListingTestLanternSite, doc.SitesUnlisted[0].SiteName)
		assert.Equal(t, siteListingTestRefusal, doc.SitesUnlisted[0].Error)
		assert.NotContains(t, keys, "site_search_error", "left out when every search answered")
		assert.Equal(t, siteListingTestLanternNote+siteListingTestIncomplete, run.stderr.String())
	})
}

// The same in drive search: the answering site's library is a result, the
// refusing site is named with its account, and the search exits 0.
//
// Validates: R-3.3.14
func TestDriveSearch_NamesASiteWhoseLibrariesCouldNotBeListed(t *testing.T) {
	fake := siteListingFake{refuseLantern: true}

	t.Run("text", func(t *testing.T) {
		run := runSiteListing(t, fake, false, runDriveSearchForSites)

		assert.Contains(t, run.stdout.String(), siteListingTestHarborLibrary)
		assert.NotContains(t, run.stdout.String(), siteListingTestLanternSite)
		assert.Equal(t, siteListingTestLanternNote+siteListingTestIncomplete, run.stderr.String())
	})

	t.Run("json", func(t *testing.T) {
		run := runSiteListing(t, fake, true, runDriveSearchForSites)

		doc, _ := decodeSiteListing(t, run)
		require.Len(t, doc.Results, 1)
		assert.Equal(t, siteListingTestHarborLibrary, doc.Results[0].CanonicalID)
		require.Len(t, doc.SitesUnlisted, 1)
		assert.Equal(t, siteListingTestAccount, doc.SitesUnlisted[0].Account)
		assert.Equal(t, siteListingTestLanternSite, doc.SitesUnlisted[0].SiteName)
		assert.Equal(t, siteListingTestRefusal, doc.SitesUnlisted[0].Error)
		assert.Equal(t, siteListingTestLanternNote+siteListingTestIncomplete, run.stderr.String())
	})
}

// When no matching site's libraries can be listed, drive search does not say
// that no site matched: sites did, and stderr names them.
//
// Validates: R-3.3.14
func TestDriveSearch_NoSiteListedIsNotNoSiteFound(t *testing.T) {
	run := runSiteListing(t, siteListingFake{refuseHarbor: true, refuseLantern: true}, false, runDriveSearchForSites)

	assert.Equal(t, "No libraries could be listed for the SharePoint sites matching \"team\".\n", run.stdout.String())
	assert.Equal(t, siteListingTestHarborNote+siteListingTestLanternNote+siteListingTestIncomplete, run.stderr.String())
}

// Both commands name the unlisted sites in one order, by account and then by
// site, whatever order the site search answered in.
//
// Validates: R-3.3.14
func TestDriveListAndSearch_NameTheUnlistedSitesInOneOrder(t *testing.T) {
	fake := siteListingFake{refuseHarbor: true, refuseLantern: true, sitesReversed: true}

	for _, tc := range siteListingCommands() {
		t.Run(tc.name, func(t *testing.T) {
			run := runSiteListing(t, fake, tc.asJSON, tc.command)

			assert.Equal(t, siteListingTestHarborNote+siteListingTestLanternNote+siteListingTestIncomplete, run.stderr.String())
			if tc.asJSON {
				doc, _ := decodeSiteListing(t, run)
				assert.Equal(t, []string{siteListingTestHarborSite, siteListingTestLanternSite}, unlistedSiteNames(&doc))
			}
		})
	}
}

// A site that comes without a display name is named by its URL name, never
// as an empty name.
//
// Validates: R-3.3.14
func TestDriveListAndSearch_NameASiteWithoutADisplayNameByItsURLName(t *testing.T) {
	fake := siteListingFake{refuseHarbor: true, harborWithoutDisplayName: true}
	note := "Note: could not list the libraries of SharePoint site harbor of user@example.com: access denied.\n"

	for _, tc := range siteListingCommands() {
		t.Run(tc.name, func(t *testing.T) {
			run := runSiteListing(t, fake, tc.asJSON, tc.command)

			assert.Equal(t, note+siteListingTestIncomplete, run.stderr.String())
		})
	}
}

// An answer whose status has no sentinel, with a request id and a page that
// is not JSON, is named by its status alone: the id and the page reach
// neither stderr nor the document, in either command or form, for an
// unlisted site and for drive list's failed site search.
//
// Validates: R-3.3.14
func TestDriveListAndSearch_AnUnrecognizedAnswerIsNamedByItsStatusAlone(t *testing.T) {
	const reason = "OneDrive answered HTTP 415"

	assertNothingOfTheAnswer := func(t *testing.T, run *siteListingRun) {
		t.Helper()

		for _, stream := range []string{run.stdout.String(), run.stderr.String()} {
			assert.NotContains(t, stream, siteListingTestRequestID)
			assert.NotContains(t, stream, siteListingTestPageText)
			assert.NotContains(t, stream, "<html>")
		}
	}

	for _, tc := range siteListingCommands() {
		t.Run(tc.name+", unlisted site", func(t *testing.T) {
			run := runSiteListing(t, siteListingFake{refuseLantern: true, unrecognizedRefusal: true}, tc.asJSON, tc.command)

			assertNothingOfTheAnswer(t, run)
			assert.Equal(t,
				"Note: could not list the libraries of SharePoint site Lantern Team of user@example.com: "+reason+".\n"+
					siteListingTestIncomplete,
				run.stderr.String())
			if tc.asJSON {
				doc, _ := decodeSiteListing(t, run)
				require.Len(t, doc.SitesUnlisted, 1)
				assert.Equal(t, reason, doc.SitesUnlisted[0].Error)
			}
		})
	}

	for _, asJSON := range []bool{false, true} {
		t.Run(fmt.Sprintf("drive list site search, json %t", asJSON), func(t *testing.T) {
			run := runSiteListing(t, siteListingFake{refuseSiteSearch: true, unrecognizedRefusal: true}, asJSON, runDriveListForSites)

			assertNothingOfTheAnswer(t, run)
			assert.Equal(t,
				"Note: could not search the SharePoint sites of user@example.com: "+reason+".\n"+siteListingTestIncomplete,
				run.stderr.String())
			if asJSON {
				doc, _ := decodeSiteListing(t, run)
				require.NotNil(t, doc.SiteSearchError)
				assert.Equal(t, siteListingTestAccount+": "+reason, *doc.SiteSearchError)
			}
		})
	}
}

// A stop that lands while a site's libraries are being listed is not a site
// that could not be listed: neither command names a site, says the listing
// is incomplete or prints a listing, and each ends with the cancellation,
// which exits 130 as every stopped command does.
//
// Validates: R-3.3.14, R-6.3.6
func TestDriveListAndSearch_AStopNamesNoSiteAndEndsWithTheCancellation(t *testing.T) {
	for _, tc := range siteListingCommands() {
		t.Run(tc.name, func(t *testing.T) {
			ctx, stop := context.WithCancel(t.Context())
			defer stop()

			signInSiteListingAccount(t)
			srv := newSiteListingGraph(t, siteListingFake{stopAtHarbor: stop})
			run, cc := startSiteListing(t, srv.URL, CLIFlags{JSON: tc.asJSON})

			err := tc.command(ctx, cc)

			require.ErrorIs(t, err, context.Canceled)
			assert.Equal(t, exitInterrupted, commandExitCode(err, true))
			assert.Empty(t, run.stderr.String())
			assert.Empty(t, run.stdout.String())
		})
	}
}

// Inside discovery a stop is not a gap either, wherever it lands: drive
// list's discovery returns no gap and writes no Warn line, and drive search's
// search of an account ends with the cancellation and names no site. The site
// loops stop at the stop: the sites after it are not asked and not named.
//
// Validates: R-3.3.14, R-6.3.6
func TestSharePointDiscovery_AStopIsNoGapAndEndsTheSiteLoop(t *testing.T) {
	stops := []struct {
		name string
		fake func(stop context.CancelFunc) siteListingFake
	}{
		{"in the site search", func(stop context.CancelFunc) siteListingFake {
			return siteListingFake{stopAtSiteSearch: stop}
		}},
		{"while a site is listed", func(stop context.CancelFunc) siteListingFake {
			return siteListingFake{stopAtHarbor: stop}
		}},
	}
	for _, tc := range stops {
		t.Run("drive list, "+tc.name, func(t *testing.T) {
			ctx, stop := context.WithCancel(t.Context())
			defer stop()

			srv := newSiteListingGraph(t, tc.fake(stop))
			var warnings bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&warnings, &slog.HandlerOptions{Level: slog.LevelWarn}))

			entries, gaps := discoverSharePointDrives(
				ctx, newTestGraphClient(t, srv.URL), &config.Config{}, siteListingTestAccount, sharePointSiteLimit, logger)

			assert.Empty(t, entries)
			assert.Equal(t, sharePointGaps{}, gaps)
			assert.Empty(t, warnings.String())
		})

		t.Run("drive search, "+tc.name, func(t *testing.T) {
			ctx, stop := context.WithCancel(t.Context())
			defer stop()

			signInSiteListingAccount(t)
			srv := newSiteListingGraph(t, tc.fake(stop))
			_, cc := startSiteListing(t, srv.URL, CLIFlags{})

			results, unlisted, authRequired, err := searchAccountSharePoint(
				ctx, driveid.MustCanonicalID(siteListingTestBusinessDrive), siteListingTestQuery,
				cc.Logger, newAuthProofRecorder(cc.Logger), cc.runtime())

			require.ErrorIs(t, err, context.Canceled)
			assert.Empty(t, results)
			assert.Empty(t, unlisted)
			assert.Empty(t, authRequired)
		})
	}
}

// A refused site search whose answer carries a request id and a page warns
// with the plain reason; the request id and the page reach the debug level
// only.
//
// Validates: R-3.3.14
func TestSharePointDiscovery_AFailedSiteSearchWarnsWithThePlainReason(t *testing.T) {
	t.Parallel()

	srv := newSiteListingGraph(t, siteListingFake{refuseSiteSearch: true, unrecognizedRefusal: true})
	var warnings, debug bytes.Buffer
	warnLogger := slog.New(slog.NewTextHandler(&warnings, &slog.HandlerOptions{Level: slog.LevelWarn}))
	debugLogger := slog.New(slog.NewTextHandler(&debug, &slog.HandlerOptions{Level: slog.LevelDebug}))

	_, gaps := discoverSharePointDrives(
		t.Context(), newTestGraphClient(t, srv.URL), &config.Config{}, siteListingTestAccount, sharePointSiteLimit, warnLogger)

	require.Len(t, gaps.FailedSiteSearches, 1)
	reason := gaps.FailedSiteSearches[0].Reason
	assert.Contains(t, warnings.String(), "SharePoint site search failed")
	assert.Contains(t, warnings.String(), reason)
	assert.NotContains(t, warnings.String(), siteListingTestRequestID)
	assert.NotContains(t, warnings.String(), siteListingTestPageText)

	discoverSharePointDrives(
		t.Context(), newTestGraphClient(t, srv.URL), &config.Config{}, siteListingTestAccount, sharePointSiteLimit, debugLogger)
	assert.Contains(t, debug.String(), siteListingTestRequestID, "the debug level keeps the whole error")
}

// --quiet keeps the notes, as it keeps warnings: without them the listing
// would read as complete.
//
// Validates: R-3.3.14
func TestDriveListAndSearch_QuietKeepsTheNotes(t *testing.T) {
	for _, tc := range siteListingCommands() {
		t.Run(tc.name, func(t *testing.T) {
			flags := CLIFlags{JSON: tc.asJSON, Quiet: true}
			run := runSiteListingWithFlags(t, siteListingFake{refuseLantern: true}, flags, tc.command)

			assert.Equal(t, siteListingTestLanternNote+siteListingTestIncomplete, run.stderr.String())
		})
	}
}

// A failed site search in drive list is said in both forms, and the drives
// that need no site search are still listed.
//
// Validates: R-3.3.14
func TestDriveList_SaysAFailedSiteSearchAndListsTheOtherDrives(t *testing.T) {
	fake := siteListingFake{refuseSiteSearch: true}
	searchNote := "Note: could not search the SharePoint sites of " + siteListingTestAccount + ": access denied.\n"

	t.Run("text", func(t *testing.T) {
		run := runSiteListing(t, fake, false, runDriveListForSites)

		assert.Contains(t, run.stdout.String(), siteListingTestBusinessDrive)
		assert.Equal(t, searchNote+siteListingTestIncomplete, run.stderr.String())
	})

	t.Run("json", func(t *testing.T) {
		run := runSiteListing(t, fake, true, runDriveListForSites)

		doc, keys := decodeSiteListing(t, run)
		assert.Equal(t, []string{siteListingTestBusinessDrive}, availableCanonicalIDs(doc.Available))
		require.NotNil(t, doc.SiteSearchError)
		assert.Equal(t, siteListingTestAccount+": "+siteListingTestRefusal, *doc.SiteSearchError)
		assert.JSONEq(t, "[]", string(keys["sites_unlisted"]))
		assert.Equal(t, searchNote+siteListingTestIncomplete, run.stderr.String())
	})
}

// drive search with two business accounts, one whose site search is refused
// and one whose search answers: the answering account's libraries are
// listed, the refused search is named on stderr and in --json
// (site_search_error), and the command exits 0.
//
// Validates: R-3.3.14
func TestDriveSearch_NamesAFailedSearchBesideAnotherAccountsResults(t *testing.T) {
	searchNote := siteListingTestOtherSearchNote

	runTwoAccounts := func(t *testing.T, asJSON bool) *siteListingRun {
		t.Helper()

		run, cc := startTwoAccountSearch(t, siteListingFake{}, asJSON)
		require.NoError(t, runDriveSearchForSites(t.Context(), cc))

		return run
	}

	t.Run("text", func(t *testing.T) {
		run := runTwoAccounts(t, false)

		assert.Contains(t, run.stdout.String(), siteListingTestHarborLibrary)
		assert.Equal(t, searchNote+siteListingTestIncomplete, run.stderr.String())
	})

	t.Run("json", func(t *testing.T) {
		run := runTwoAccounts(t, true)

		doc, keys := decodeSiteListing(t, run)
		assert.Len(t, doc.Results, 2)
		require.NotNil(t, doc.SiteSearchError)
		assert.Equal(t, siteListingTestOtherAccount+": "+siteListingTestRefusal, *doc.SiteSearchError)
		assert.JSONEq(t, "[]", string(keys["sites_unlisted"]))
		assert.Equal(t, searchNote+siteListingTestIncomplete, run.stderr.String())
	})
}

// startTwoAccountSearch signs in the made-up account, which answers as
// userFake says, and a second made-up business account whose site search is
// refused, and returns the command context of a drive search against both.
func startTwoAccountSearch(t *testing.T, userFake siteListingFake, asJSON bool) (*siteListingRun, *CLIContext) {
	t.Helper()

	signInSiteListingAccount(t)
	other := driveid.MustCanonicalID("business:" + siteListingTestOtherAccount)
	seedCatalogAccount(t, other, nil)
	writeAccessTokenFile(t, other, siteListingTestOtherSignIn)

	answering := siteListingHandler(t, userFake)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+siteListingTestOtherSignIn {
			answering(w, r)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case testGraphMePath:
			writeTestResponse(t, w, `{"id":"user-2","displayName":"Other Example","mail":"other@example.com"}`)
		case testGraphSitesPath:
			w.WriteHeader(http.StatusForbidden)
			writeTestResponse(t, w, `{"error":{"code":"accessDenied","message":"Access denied"}}`)
		default:
			assert.Fail(t, "unexpected graph path", "path=%s", r.URL.Path)
			http.Error(w, "unexpected graph path", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)

	return startSiteListing(t, srv.URL, CLIFlags{JSON: asJSON})
}

// A drive search that fails, because no account's search found a result,
// still names before its error each failed search with its account and each
// site another account could not list. With no listing printed it does not
// say one is incomplete, and it exits 1 with stdout empty.
//
// Validates: R-3.3.14
func TestDriveSearch_FailingStillNamesEverySourceThatDidNotAnswer(t *testing.T) {
	userSearchNote := "Note: could not search the SharePoint sites of " + siteListingTestAccount + ": access denied.\n"

	cases := []struct {
		name     string
		userFake siteListingFake
		stderr   string
	}{
		{
			name:     "both searches refused",
			userFake: siteListingFake{refuseSiteSearch: true},
			stderr:   siteListingTestOtherSearchNote + userSearchNote,
		},
		{
			name:     "one search refused, every site of the other account unlisted",
			userFake: siteListingFake{refuseHarbor: true, refuseLantern: true},
			stderr:   siteListingTestOtherSearchNote + siteListingTestHarborNote + siteListingTestLanternNote,
		},
	}
	for _, tc := range cases {
		for _, asJSON := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, json %t", tc.name, asJSON), func(t *testing.T) {
				run, cc := startTwoAccountSearch(t, tc.userFake, asJSON)

				err := runDriveSearchForSites(t.Context(), cc)

				require.ErrorIs(t, err, graph.ErrForbidden)
				assert.Equal(t, exitFailure, commandExitCode(err, false))
				assert.Equal(t, tc.stderr, run.stderr.String())
				assert.Empty(t, run.stdout.String())
			})
		}
	}
}

// With every site answering, both commands print what they printed before,
// byte for byte, nothing on stderr, and in --json an empty sites_unlisted
// and no site_search_error.
//
// Validates: R-3.3.14
func TestDriveListAndSearch_EverySiteAnsweringPrintsWhatItDidBefore(t *testing.T) {
	everyAnswer := siteListingFake{}

	t.Run("drive list text", func(t *testing.T) {
		run := runSiteListing(t, everyAnswer, false, runDriveListForSites)

		assert.Equal(t, "Available drives (not configured):\n"+
			"  business:user@example.com\n"+
			"  sharepoint:user@example.com:harbor:Documents (Harbor Team)\n"+
			"  sharepoint:user@example.com:lantern:Reports (Lantern Team)\n"+
			"\nRun 'onedrive-go drive add <canonical-id>' to add a drive.\n",
			run.stdout.String())
		assert.Empty(t, run.stderr.String())
	})

	t.Run("drive search text", func(t *testing.T) {
		run := runSiteListing(t, everyAnswer, false, runDriveSearchForSites)

		assert.Equal(t, "SharePoint sites matching \"team\":\n"+
			"\n  Harbor Team (https://example.com/sites/harbor)\n"+
			"    sharepoint:user@example.com:harbor:Documents\n"+
			"\n"+
			"\n  Lantern Team (https://example.com/sites/lantern)\n"+
			"    sharepoint:user@example.com:lantern:Reports\n"+
			"\nRun 'onedrive-go drive add <canonical-id>' to add a drive.\n",
			run.stdout.String())
		assert.Empty(t, run.stderr.String())
	})

	t.Run("drive list json", func(t *testing.T) {
		run := runSiteListing(t, everyAnswer, true, runDriveListForSites)

		doc, keys := decodeSiteListing(t, run)
		assert.Len(t, doc.Available, 3)
		assert.JSONEq(t, "[]", string(keys["sites_unlisted"]))
		assert.NotContains(t, keys, "site_search_error", "left out when every search answered")
		assert.Empty(t, run.stderr.String())
	})

	t.Run("drive search json", func(t *testing.T) {
		run := runSiteListing(t, everyAnswer, true, runDriveSearchForSites)

		doc, keys := decodeSiteListing(t, run)
		assert.Len(t, doc.Results, 2)
		assert.JSONEq(t, "[]", string(keys["sites_unlisted"]))
		assert.NotContains(t, keys, "site_search_error", "left out when every search answered")
		assert.Empty(t, run.stderr.String())
	})
}

// drive list searches the sites of every business account, so
// site_search_error names whose search failed, each one once.
//
// Validates: R-3.3.14
func TestSiteSearchErrorText_NamesEachAccountWhoseSearchFailed(t *testing.T) {
	assert.Empty(t, siteSearchErrorText(nil))
	assert.Equal(t, "user@example.com: access denied", siteSearchErrorText([]failedSiteSearch{
		{Account: siteListingTestAccount, Reason: siteListingTestRefusal},
	}))
	assert.Equal(t, "other@example.com: OneDrive is limiting requests; user@example.com: access denied", siteSearchErrorText([]failedSiteSearch{
		{Account: siteListingTestOtherAccount, Reason: "OneDrive is limiting requests"},
		{Account: siteListingTestAccount, Reason: siteListingTestRefusal},
	}))
}

// The gaps are put in one order whatever order they were found in: by
// account, then by site, so two accounts' sites of one name never swap
// between runs.
//
// Validates: R-3.3.14
func TestSortSharePointGaps_OrdersByAccountThenSite(t *testing.T) {
	gaps := sharePointGaps{
		UnlistedSites: []unlistedSite{
			{Account: siteListingTestAccount, SiteName: siteListingTestHarborSite, Error: siteListingTestRefusal},
			{Account: siteListingTestOtherAccount, SiteName: siteListingTestLanternSite, Error: siteListingTestRefusal},
			{Account: siteListingTestOtherAccount, SiteName: siteListingTestHarborSite, Error: siteListingTestRefusal},
		},
		FailedSiteSearches: []failedSiteSearch{
			{Account: siteListingTestAccount, Reason: siteListingTestRefusal},
			{Account: siteListingTestOtherAccount, Reason: siteListingTestRefusal},
		},
	}

	sortSharePointGaps(&gaps)

	assert.Equal(t, []unlistedSite{
		{Account: siteListingTestOtherAccount, SiteName: siteListingTestHarborSite, Error: siteListingTestRefusal},
		{Account: siteListingTestOtherAccount, SiteName: siteListingTestLanternSite, Error: siteListingTestRefusal},
		{Account: siteListingTestAccount, SiteName: siteListingTestHarborSite, Error: siteListingTestRefusal},
	}, gaps.UnlistedSites)
	assert.Equal(t, []failedSiteSearch{
		{Account: siteListingTestOtherAccount, Reason: siteListingTestRefusal},
		{Account: siteListingTestAccount, Reason: siteListingTestRefusal},
	}, gaps.FailedSiteSearches)
}

// The reason a source did not answer is one of the code's own phrases,
// chosen by the kind of failure; nothing of the answer (request id, code,
// message, body) or of the request (its URL) reaches it.
//
// Validates: R-3.3.14
func TestSharePointGapReason_SaysTheKindOfFailureInItsOwnWords(t *testing.T) {
	const madeUpURL = "https://example.com/sites/site-lantern/drives?$skiptoken=made-up-skiptoken"

	answer := func(status int, sentinel error, innerCodes ...string) error {
		return fmt.Errorf("listing site drives: %w", &graph.GraphError{
			StatusCode: status,
			RequestID:  siteListingTestRequestID,
			Code:       "madeUpCode",
			InnerCodes: innerCodes,
			Message:    siteListingTestPageText,
			RawBody:    siteListingTestPageText,
			Err:        sentinel,
		})
	}
	beforeTheAnswer := func(cause error) error {
		return &url.Error{Op: "Get", URL: madeUpURL, Err: cause}
	}

	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"rejected sign-in":     {answer(http.StatusUnauthorized, graph.ErrUnauthorized), "sign-in required"},
		"read-only window":     {answer(http.StatusForbidden, graph.ErrForbidden, "serviceReadOnly"), "OneDrive is temporarily read-only"},
		"refused":              {answer(http.StatusForbidden, graph.ErrForbidden), "access denied"},
		"not found":            {answer(http.StatusNotFound, graph.ErrNotFound), "not found"},
		"throttled":            {answer(http.StatusTooManyRequests, graph.ErrThrottled), "OneDrive is limiting requests"},
		"server problem":       {answer(http.StatusServiceUnavailable, graph.ErrServerError), "OneDrive had a temporary server problem"},
		"status, no sentinel":  {answer(http.StatusUnsupportedMediaType, nil), "OneDrive answered HTTP 415"},
		"other 4xx":            {answer(http.StatusBadRequest, graph.ErrBadRequest), "OneDrive answered HTTP 400"},
		"timeout":              {beforeTheAnswer(headerTimeoutError{}), "OneDrive did not answer in time"},
		"connection refused":   {beforeTheAnswer(&net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}), "the connection to OneDrive failed"},
		"connection dropped":   {beforeTheAnswer(http2StreamError{}), "the connection to OneDrive failed"},
		"next link off-host":   {fmt.Errorf("graph: next link %s is not under the base URL", madeUpURL), "the request failed"},
		"unreadable answer":    {errors.New("graph: decoding site drives: invalid character 'm' looking for beginning of value"), "the request failed"},
		"nothing but the text": {errors.New(siteListingTestPageText), "the request failed"},
	} {
		t.Run(name, func(t *testing.T) {
			got := sharePointGapReason(tc.err)

			assert.Equal(t, tc.want, got)
			assert.NotContains(t, got, siteListingTestRequestID)
			assert.NotContains(t, got, siteListingTestPageText)
			assert.NotContains(t, got, "madeUpCode")
			assert.NotContains(t, got, "example.com")
		})
	}
}
