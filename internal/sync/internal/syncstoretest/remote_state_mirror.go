package syncstoretest

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
)

// ReadRemoteStateRow is the remote_state row of itemID, nil when the store
// has none. It reads through the store's own lookup (GetRemoteStateByID),
// so a test sees the row as the store returns it.
func ReadRemoteStateRow(t *testing.T, store *syncstore.SyncStore, itemID string) *syncdomain.RemoteStateRow {
	t.Helper()

	row, found, err := store.GetRemoteStateByID(t.Context(), driveid.ID{}, itemID)
	require.NoError(t, err)
	if !found {
		return nil
	}

	return row
}
