package cli

import "context"

func noStatusLiveOverlay(
	_ context.Context,
	_ *CLIContext,
	_ []accountCatalogEntry,
) map[string]statusAccountLiveOverlay {
	return nil
}
