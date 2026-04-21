package sync

import (
	"context"
	"fmt"
)

func (e *Engine) prepareRunOnceBaseline(
	ctx context.Context,
	runner *oneShotRunner,
) (*Baseline, error) {
	if err := runner.prepareRunOnceState(ctx); err != nil {
		return nil, err
	}
	bl, err := e.baseline.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("sync: loading baseline after startup preparation: %w", err)
	}

	return bl, nil
}

func (r *oneShotRunner) prepareRunOnceState(ctx context.Context) error {
	eng := r.engine
	flow := r.engineFlow

	hasAccountAuthRequirement, err := eng.hasPersistedAccountAuthRequirement()
	if err != nil {
		return err
	}

	proof, proofErr := eng.proveDriveIdentity(ctx)
	if proofErr != nil {
		if !hasAccountAuthRequirement {
			return proofErr
		}
	}

	normalizeErr := flow.scopeController().normalizePersistedScopes(ctx, nil)
	if normalizeErr != nil {
		return fmt.Errorf("sync: normalizing persisted scopes: %w", normalizeErr)
	}
	authNormalizeErr := eng.normalizePersistedAccountAuthRequirement(ctx, hasAccountAuthRequirement, proof, proofErr)
	if authNormalizeErr != nil {
		return authNormalizeErr
	}
	if proofErr != nil {
		return proofErr
	}
	eng.logVerifiedDrive(proof)

	if _, err := eng.baseline.Load(ctx); err != nil {
		return fmt.Errorf("sync: loading baseline: %w", err)
	}

	return nil
}
