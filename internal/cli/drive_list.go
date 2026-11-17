package cli

import (
	"context"
	"fmt"
)

func runDriveListWithContext(ctx context.Context, cc *CLIContext, showAll bool) error {
	snapshot, err := loadDriveListSnapshot(ctx, cc, showAll)
	if err != nil {
		return err
	}
	// A stop during discovery is not a source that did not answer: the
	// command ends with it before printing, as every command does.
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("listing drives: %w", err)
	}

	printSharePointGapNotes(cc, snapshot.SharePointGaps)

	if cc.Flags.JSON {
		return printDriveListJSON(
			cc.Output(),
			snapshot.Configured,
			snapshot.Available,
			snapshot.AccountsRequiringAuth,
			snapshot.AccountsDegraded,
			snapshot.SharePointGaps,
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
