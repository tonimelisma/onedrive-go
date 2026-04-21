package sync

import "context"

func (r *oneShotRunner) dispatchInitialReadyActions(
	ctx context.Context,
	bl *Baseline,
	depGraph *DepGraph,
	initialReady []*TrackedAction,
	report *Report,
) ([]*TrackedAction, bool, error) {
	initialOutbox, err := r.drainPublicationReadyActions(ctx, nil, bl, nil, initialReady)
	if err != nil {
		r.completeOutboxAsShutdown(initialOutbox)
		report.Succeeded, report.Failed, report.Errors = r.resultStats()
		report.Errors = append(report.Errors, err)
		return nil, true, err
	}

	if depGraph.InFlightCount() == 0 {
		r.logFailureSummary()
		report.Succeeded, report.Failed, report.Errors = r.resultStats()
		return nil, true, nil
	}

	return initialOutbox, false, nil
}
