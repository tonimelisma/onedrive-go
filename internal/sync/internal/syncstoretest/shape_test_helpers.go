package syncstoretest

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
)

func ReadObservationStateForTest(
	tb testing.TB,
	store *syncstore.SyncStore,
	ctx context.Context,
) syncdomain.ObservationState {
	tb.Helper()

	state, err := store.ReadObservationState(ctx)
	require.NoError(tb, err)
	require.NotNil(tb, state)
	return *state
}
