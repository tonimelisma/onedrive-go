package cli

import "context"

func noStatusLiveOverlay(
	_ context.Context,
	_ *CLIContext,
	_ accountViewSnapshot,
) map[string]statusAccountLiveOverlay {
	return nil
}
