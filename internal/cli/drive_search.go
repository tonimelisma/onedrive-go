package cli

import (
	"cmp"
	"context"
	"encoding/json"
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

	snapshot, err := loadAccountCatalogSnapshotWithBestEffortIdentityRefresh(ctx, cc)
	if err != nil {
		return err
	}

	businessTokens := searchableBusinessTokenIDs(snapshot.Catalog, cc.Flags.Account)
	businessAuthRequired := authRequirements(snapshot, func(entry accountCatalogEntry) bool {
		if cc.Flags.Account != "" && entry.Email != cc.Flags.Account {
			return false
		}
		return entry.DriveType == driveid.DriveTypeBusiness
	})
	if len(businessTokens) == 0 && len(businessAuthRequired) == 0 {
		if cc.Flags.Account != "" {
			return fmt.Errorf("no business account found for %s — run 'onedrive-go login' first", cc.Flags.Account)
		}

		return fmt.Errorf("no business accounts found — SharePoint search requires a business account")
	}

	var (
		results                []driveSearchResult
		discoveredAuthRequired []accountAuthRequirement
	)
	for _, tokenCID := range businessTokens {
		accountResults, accountAuthRequired := searchAccountSharePoint(
			ctx,
			tokenCID,
			query,
			logger,
			recorder,
			cc.GraphBaseURL,
			cc.runtime(),
		)
		results = append(results, accountResults...)
		discoveredAuthRequired = append(discoveredAuthRequired, accountAuthRequired...)
	}
	authRequired := mergeAuthRequirements(businessAuthRequired, discoveredAuthRequired)

	if cc.Flags.JSON {
		return printDriveSearchJSON(cc.Output(), results, authRequired)
	}

	return printDriveSearchText(cc.Output(), results, query, authRequired)
}

// driveSearchResult represents a search result for JSON output.
type driveSearchResult struct {
	CanonicalID string `json:"canonical_id"`
	SiteName    string `json:"site_name"`
	LibraryName string `json:"library_name"`
	WebURL      string `json:"web_url,omitempty"`
}

type driveSearchJSONOutput struct {
	Results               []driveSearchResult      `json:"results"`
	AccountsRequiringAuth []accountAuthRequirement `json:"accounts_requiring_auth,omitempty"`
}

// sharePointSearchLimit caps the number of SharePoint sites returned per search query.
const sharePointSearchLimit = 50

// searchAccountSharePoint searches SharePoint sites for a single business account.
func searchAccountSharePoint(
	ctx context.Context,
	tokenCID driveid.CanonicalID,
	query string,
	logger *slog.Logger,
	recorder *authProofRecorder,
	baseURL string,
	runtime *driveops.SessionRuntime,
) ([]driveSearchResult, []accountAuthRequirement) {
	tokenPath := config.DriveTokenPath(tokenCID)
	if tokenPath == "" {
		return nil, nil
	}

	ts, err := graph.TokenSourceFromPath(ctx, tokenPath, logger)
	if err != nil {
		logger.Debug("skipping token for search", "token", tokenCID.String(), "error", err)

		return nil, []accountAuthRequirement{tokenDiscoveryAuthRequirement(tokenCID, err, logger)}
	}

	client, err := newGraphClientWithHTTP(
		baseURL,
		runtime.BootstrapMeta(),
		ts,
		logger,
	)
	if err != nil {
		logger.Debug("skipping token for search", "token", tokenCID.String(), "error", err)

		return nil, nil
	}
	attachAccountAuthProof(client, recorder, tokenCID.Email(), "drive-search")

	sites, err := client.SearchSites(ctx, query, sharePointSearchLimit)
	if err != nil {
		logger.Warn("SharePoint search failed", "account", tokenCID.Email(), "error", err)

		if errors.Is(err, graph.ErrUnauthorized) {
			return nil, []accountAuthRequirement{tokenAuthRequirement(tokenCID, authReasonSyncAuthRejected, logger)}
		}

		return nil, nil
	}

	email := tokenCID.Email()
	var results []driveSearchResult

	for _, site := range sites {
		siteDrives, driveErr := client.SiteDrives(ctx, site.ID)
		if driveErr != nil {
			logger.Debug("failed to list drives for site", "site", site.Name, "error", driveErr)

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

	return results, nil
}

func printDriveSearchJSON(
	w io.Writer,
	results []driveSearchResult,
	authRequiredOpt ...[]accountAuthRequirement,
) error {
	authRequired := optionalAuthRequirements(authRequiredOpt)
	if results == nil {
		results = []driveSearchResult{}
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	if err := enc.Encode(driveSearchJSONOutput{
		Results:               results,
		AccountsRequiringAuth: authRequired,
	}); err != nil {
		return fmt.Errorf("encoding JSON output: %w", err)
	}

	return nil
}

func printDriveSearchText(
	w io.Writer,
	results []driveSearchResult,
	query string,
	authRequiredOpt ...[]accountAuthRequirement,
) error {
	authRequired := optionalAuthRequirements(authRequiredOpt)

	if len(results) == 0 {
		if len(authRequired) == 0 {
			return writef(w, "No SharePoint sites found matching %q.\n", query)
		}

		if err := printDriveSearchAuthSection(w, authRequired, false); err != nil {
			return err
		}
		return writef(w, "No SharePoint sites found matching %q in searchable business accounts.\n", query)
	}

	if err := printDriveSearchAuthSection(w, authRequired, len(results) > 0); err != nil {
		return err
	}

	return printDriveSearchResults(w, sortedDriveSearchResults(results), query)
}

func optionalAuthRequirements(authRequiredOpt [][]accountAuthRequirement) []accountAuthRequirement {
	if len(authRequiredOpt) == 0 {
		return nil
	}

	return authRequiredOpt[0]
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
	if result == nil || currentSite == nil {
		return nil
	}

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
