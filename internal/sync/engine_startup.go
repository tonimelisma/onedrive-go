package sync

import (
	"context"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// runStartup runs the shared startup proof/normalization sequence used by
// one-shot and watch before they diverge into their own runtime shells. The
// engine proves its drive identity and settles its account's sign-in
// requirement itself; the run's flow normalizes the persisted scopes and loads
// the baseline in between.
func (e *Engine) runStartup(
	ctx context.Context,
	flow *engineFlow,
) (syncdomain.BaselineView, error) {
	hasAccountAuthRequirement, err := e.hasPersistedAccountAuthRequirement()
	if err != nil {
		return nil, err
	}

	proof, proofErr := e.proveDriveIdentity(ctx)
	if proofErr != nil && !hasAccountAuthRequirement {
		return nil, proofErr
	}

	normalizeErr := flow.normalizePersistedScopes(ctx)
	if normalizeErr != nil {
		return nil, fmt.Errorf("sync: normalizing persisted scopes: %w", normalizeErr)
	}
	authNormalizeErr := e.normalizePersistedAccountAuthRequirement(ctx, hasAccountAuthRequirement, proof, proofErr)
	if authNormalizeErr != nil {
		return nil, authNormalizeErr
	}
	if proofErr != nil {
		return nil, proofErr
	}
	e.logVerifiedDrive(proof)

	bl, err := flow.deps.store.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("sync: loading baseline after startup preparation: %w", err)
	}

	return bl, nil
}
