package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

func runDriveSearchWithContext(ctx context.Context, cc *CLIContext, query string) error {
	logger := cc.Logger
	recorder := newAuthProofRecorder(logger)

	snapshot, err := loadAccountViewSnapshotWithBestEffortIdentityRefresh(ctx, cc)
	if err != nil {
		return err
	}

	businessTokens := searchableBusinessTokenIDs(snapshot.Accounts, cc.Flags.Account)
	businessAuthRequired := authRequirements(snapshot, func(entry accountView) bool {
		if cc.Flags.Account != "" && entry.Email != cc.Flags.Account {
			return false
		}
		return entry.DriveType == driveid.DriveTypeBusiness
	})
	if len(businessTokens) == 0 && len(businessAuthRequired) == 0 {
		if cc.Flags.Account != "" {
			return fmt.Errorf("no business account found for %s — run 'onedrive-go login' first", cc.Flags.Account)
		}

		return errors.New("no business accounts found — SharePoint search requires a business account")
	}

	var (
		results                []driveSearchResult
		gaps                   sharePointGaps
		discoveredAuthRequired []accountAuthRequirement
		searchErrs             []error
	)
	for _, tokenCID := range businessTokens {
		accountResults, accountUnlisted, accountAuthRequired, searchErr := searchAccountSharePoint(
			ctx,
			tokenCID,
			query,
			logger,
			recorder,
			cc.runtime(),
		)
		// A stop is not a failed search or a site that could not be listed:
		// the command ends with it, as every command does.
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("searching SharePoint: %w", err)
		}
		if searchErr != nil {
			searchErrs = append(searchErrs, searchErr)
			gaps.FailedSiteSearches = append(gaps.FailedSiteSearches,
				failedSiteSearch{Account: tokenCID.Email(), Reason: sharePointGapReason(searchErr)})
		}
		results = append(results, accountResults...)
		gaps.UnlistedSites = append(gaps.UnlistedSites, accountUnlisted...)
		discoveredAuthRequired = append(discoveredAuthRequired, accountAuthRequired...)
	}
	// With no result to show, a failed search fails the command. With several
	// accounts the notes first name each failed search by its account and each
	// site another account could not list, so no gap goes unsaid. Beside
	// another account's results a failed search is named like an unlisted site.
	if len(results) == 0 && len(searchErrs) > 0 {
		if len(businessTokens) > 1 {
			sortSharePointGaps(&gaps)
			printSharePointSourceNotes(cc, gaps)
		}
		return searchErrs[0]
	}
	authRequired := mergeAuthRequirements(businessAuthRequired, discoveredAuthRequired)
	sortSharePointGaps(&gaps)
	printSharePointGapNotes(cc, gaps)

	if cc.Flags.JSON {
		return printDriveSearchJSON(cc.Output(), results, authRequired, gaps)
	}

	return printDriveSearchText(cc.Output(), results, query, authRequired, gaps.UnlistedSites)
}

// driveSearchResult represents a search result for JSON output.
type driveSearchResult struct {
	CanonicalID string `json:"canonical_id"`
	SiteName    string `json:"site_name"`
	LibraryName string `json:"library_name"`
	WebURL      string `json:"web_url,omitempty"`
}

// driveSearchJSONOutput is `drive search --json`. SitesUnlisted names each
// matching site whose libraries could not be listed, and SiteSearchError each
// account whose search failed beside another account's results; both empty,
// the results are complete (R-3.3.14).
type driveSearchJSONOutput struct {
	Results               []driveSearchResult      `json:"results"`
	AccountsRequiringAuth []accountAuthRequirement `json:"accounts_requiring_auth"`
	SitesUnlisted         []unlistedSite           `json:"sites_unlisted"`
	SiteSearchError       string                   `json:"site_search_error,omitempty"`
}

func (out driveSearchJSONOutput) withEmptyLists() driveSearchJSONOutput {
	out.Results = emptyIfNil(out.Results)
	out.AccountsRequiringAuth = emptyIfNil(out.AccountsRequiringAuth)
	out.SitesUnlisted = emptyIfNil(out.SitesUnlisted)

	return out
}

// sharePointSearchLimit caps the number of SharePoint sites returned per search query.
const sharePointSearchLimit = 50

// searchAccountSharePoint searches SharePoint sites for a single business
// account. A matching site whose libraries could not be listed is returned as
// unlisted, for the command to name; a stop is returned as an error.
func searchAccountSharePoint(
	ctx context.Context,
	tokenCID driveid.CanonicalID,
	query string,
	logger *slog.Logger,
	recorder *authProofRecorder,
	runtime *driveops.SessionRuntime,
) ([]driveSearchResult, []unlistedSite, []accountAuthRequirement, error) {
	tokenPath := config.DriveTokenPath(tokenCID)
	if tokenPath == "" {
		return nil, nil, nil, nil
	}

	client, err := runtime.BootstrapClient(tokenPath)
	if err != nil {
		logger.Debug("skipping token for search", "token", tokenCID.String(), "error", err)

		return nil, nil, []accountAuthRequirement{tokenDiscoveryAuthRequirement(tokenCID, err, logger)}, nil
	}
	attachAccountAuthProof(client, recorder, tokenCID, "drive-search")

	sites, err := client.SearchSites(ctx, query, sharePointSearchLimit)
	if err != nil {
		if errors.Is(err, graph.ErrUnauthorized) {
			logger.Debug("SharePoint search rejected the saved login", "account", tokenCID.Email(), "error", err)
			return nil, nil, []accountAuthRequirement{tokenAuthRequirement(tokenCID, authReasonSyncAuthRejected, logger)}, nil
		}

		// A failed search is not an empty result: report it so the command
		// fails, or names it beside other accounts' results, instead of
		// printing "No SharePoint sites found".
		return nil, nil, nil, fmt.Errorf("searching SharePoint for %s: %w", tokenCID.Email(), err)
	}

	email := tokenCID.Email()
	var (
		results  []driveSearchResult
		unlisted []unlistedSite
	)

	for _, site := range sites {
		siteDrives, driveErr := client.SiteDrives(ctx, site.ID)
		if driveErr != nil {
			logger.Debug("failed to list drives for site", "site", site.Name, "error", driveErr)
			// A stop is not a site that failed: it ends the search, and the
			// command ends with it.
			if ctx.Err() != nil {
				return nil, nil, nil, fmt.Errorf("listing SharePoint libraries for %s: %w", email, driveErr)
			}
			unlisted = append(unlisted, newUnlistedSite(email, site, driveErr))

			continue
		}

		for _, d := range siteDrives {
			cid, cidErr := driveid.ConstructSharePoint(email, site.Name, d.Name)
			if cidErr != nil {
				logger.Debug("skipping SharePoint drive with invalid ID",
					"site", site.Name, "library", d.Name, "error", cidErr)

				continue
			}

			results = append(results, driveSearchResult{
				CanonicalID: cid.String(),
				SiteName:    site.DisplayName,
				LibraryName: d.Name,
				WebURL:      site.WebURL,
			})
		}
	}

	return results, unlisted, nil, nil
}

func printDriveSearchJSON(w io.Writer, results []driveSearchResult, authRequired []accountAuthRequirement, gaps sharePointGaps) error {
	return writeJSONDocument(w, driveSearchJSONOutput{
		Results:               results,
		AccountsRequiringAuth: authRequired,
		SitesUnlisted:         gaps.UnlistedSites,
		SiteSearchError:       siteSearchErrorText(gaps.FailedSiteSearches),
	}.withEmptyLists())
}

// printDriveSearchText prints the results. With none, it says why: no site
// matched, or (unlisted holds sites) sites matched whose libraries could not
// be listed, which the stderr notes name.
func printDriveSearchText(
	w io.Writer,
	results []driveSearchResult,
	query string,
	authRequired []accountAuthRequirement,
	unlisted []unlistedSite,
) error {
	if len(results) == 0 {
		if len(authRequired) == 0 && len(unlisted) == 0 {
			return writef(w, "No SharePoint sites found matching %q.\n", query)
		}

		if err := printDriveSearchAuthSection(w, authRequired, false); err != nil {
			return err
		}
		if len(unlisted) > 0 {
			return writef(w, "No libraries could be listed for the SharePoint sites matching %q.\n", query)
		}
		return writef(w, "No SharePoint sites found matching %q in searchable business accounts.\n", query)
	}

	if err := printDriveSearchAuthSection(w, authRequired, true); err != nil {
		return err
	}

	return printDriveSearchResults(w, sortedDriveSearchResults(results), query)
}

func printDriveSearchAuthSection(
	w io.Writer,
	authRequired []accountAuthRequirement,
	hasResults bool,
) error {
	if len(authRequired) == 0 {
		return nil
	}

	if err := printAccountAuthRequirementsText(w, authRequired); err != nil {
		return err
	}

	if !hasResults {
		return nil
	}

	return writeln(w)
}

func sortedDriveSearchResults(results []driveSearchResult) []driveSearchResult {
	sorted := slices.Clone(results)
	slices.SortFunc(sorted, func(a, b driveSearchResult) int {
		if c := cmp.Compare(a.SiteName, b.SiteName); c != 0 {
			return c
		}

		return cmp.Compare(a.LibraryName, b.LibraryName)
	})

	return sorted
}

func printDriveSearchResults(
	w io.Writer,
	results []driveSearchResult,
	query string,
) error {
	if err := writef(w, "SharePoint sites matching %q:\n", query); err != nil {
		return err
	}

	currentSite := ""

	for i := range results {
		if err := printDriveSearchSiteEntry(w, &results[i], &currentSite); err != nil {
			return err
		}
	}

	return writef(w, "\nRun 'onedrive-go drive add <canonical-id>' to add a drive.\n")
}

func printDriveSearchSiteEntry(
	w io.Writer,
	result *driveSearchResult,
	currentSite *string,
) error {
	if result.SiteName != *currentSite {
		if *currentSite != "" {
			if err := writeln(w); err != nil {
				return err
			}
		}

		*currentSite = result.SiteName
		label := result.SiteName
		if result.WebURL != "" {
			label = fmt.Sprintf("%s (%s)", result.SiteName, result.WebURL)
		}

		if err := writef(w, "\n  %s\n", label); err != nil {
			return err
		}
	}

	return writef(w, "    %s\n", result.CanonicalID)
}
