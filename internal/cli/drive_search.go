package cli

import (
	"context"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
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
