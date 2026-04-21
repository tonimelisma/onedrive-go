package sync

import "context"

func (r *oneShotRunner) dispatchInitialReadyActions(
	ctx context.Context,
	bl *Baseline,
	prepared *PreparedCurrentPlan,
	report *Report,
) ([]*TrackedAction, bool, error) {
	initialOutbox, dispatched, err := r.startPreparedRuntime(ctx, prepared, bl, nil)
	if err != nil {
		report.Succeeded, report.Failed, report.Errors = r.resultStats()
		report.Errors = append(report.Errors, err)
		return nil, true, err
	}

	if !dispatched {
		r.logFailureSummary()
		report.Succeeded, report.Failed, report.Errors = r.resultStats()
		return nil, true, nil
	}

	return initialOutbox, false, nil
}
