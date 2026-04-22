package sync

import (
	"context"
	"fmt"
)

// prepareStartupBaseline runs the shared startup proof/normalization sequence
// used by one-shot and watch before they diverge into their own runtime shells.
func (flow *engineFlow) prepareStartupBaseline(
	ctx context.Context,
	watch *watchRuntime,
) (*Baseline, error) {
	eng := flow.engine

	hasAccountAuthRequirement, err := eng.hasPersistedAccountAuthRequirement()
	if err != nil {
		return nil, err
	}

	proof, proofErr := eng.proveDriveIdentity(ctx)
	if proofErr != nil && !hasAccountAuthRequirement {
		return nil, proofErr
	}

	normalizeErr := flow.normalizePersistedScopes(ctx, watch)
	if normalizeErr != nil {
		return nil, fmt.Errorf("sync: normalizing persisted scopes: %w", normalizeErr)
	}
	authNormalizeErr := eng.normalizePersistedAccountAuthRequirement(ctx, hasAccountAuthRequirement, proof, proofErr)
	if authNormalizeErr != nil {
		return nil, authNormalizeErr
	}
	if proofErr != nil {
		return nil, proofErr
	}
	eng.logVerifiedDrive(proof)

	bl, err := eng.baseline.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("sync: loading baseline after startup preparation: %w", err)
	}

	return bl, nil
}
