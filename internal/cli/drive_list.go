package cli

import (
	"context"
)

func runDriveListWithContext(ctx context.Context, cc *CLIContext, showAll bool) error {
	snapshot, err := loadDriveListSnapshot(ctx, cc, showAll)
	if err != nil {
		return err
	}

	if cc.Flags.JSON {
		return printDriveListJSON(
			cc.Output(),
			snapshot.Configured,
			snapshot.Available,
			snapshot.AccountsRequiringAuth,
			snapshot.AccountsDegraded,
		)
	}

	return printDriveListText(
		cc.Output(),
		snapshot.Configured,
		snapshot.Available,
		snapshot.AccountsRequiringAuth,
		snapshot.AccountsDegraded,
	)
}
