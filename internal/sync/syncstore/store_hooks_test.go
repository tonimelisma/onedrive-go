package syncstore

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// A store opened with zero hooks behaves as production does: it opens
// through the SQLite driver, and its crash points do nothing.
//
// Validates: R-6.10.29
func TestHooks_ZeroValueIsProduction(t *testing.T) {
	t.Parallel()

	assert.Equal(t, SqliteDriverName, Hooks{}.driverName(), "the store opens through SQLite")

	dbPath := filepath.Join(t.TempDir(), "state.db")
	store, err := OpenEngineSyncStore(synctest.TestContext(t), dbPath, synctest.TestLogger(t), Hooks{})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, store.Close(context.WithoutCancel(t.Context()))) })

	for _, point := range syncfixture.AllCrashPoints() {
		require.NoError(t, store.reachCrashPoint(point), "crash point %s does nothing", point)
	}
}
