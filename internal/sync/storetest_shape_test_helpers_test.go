package sync

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func readObservationStateForTest(
	tb testing.TB,
	store *SyncStore,
	ctx context.Context,
) ObservationState {
	tb.Helper()

	state, err := store.ReadObservationState(ctx)
	require.NoError(tb, err)
	require.NotNil(tb, state)
	return *state
}
