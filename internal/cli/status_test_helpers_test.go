package cli

import "context"

func noStatusLiveOverlay(
	_ context.Context,
	_ *CLIContext,
	_ []accountView,
) map[string]statusAccountLiveOverlay {
	return nil
}
