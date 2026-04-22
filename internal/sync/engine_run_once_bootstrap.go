package sync

import "context"

func (e *Engine) prepareRunOnceBaseline(
	ctx context.Context,
	runner *oneShotRunner,
) (*Baseline, error) {
	return runner.prepareStartupBaseline(ctx, nil)
}
