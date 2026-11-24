package sync

import (
	stdsync "sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncstoretest"
)

// SQLite's PRAGMA synchronous values.
const (
	sqliteSynchronousOff  = 0
	sqliteSynchronousFull = 2
)

// realFsyncTests names the tests whose engines open their stores as
// production does, synchronous=FULL; every other test engine skips the waits
// for the disk (syncstore.Hooks.SkipFsync).
//
//nolint:gochecknoglobals // a registry test engines consult by test name; newTestEngineInputs has no other way to learn its test's setting
var realFsyncTests stdsync.Map

// keepRealFsync makes every engine built under t, or its subtests, wait for
// the disk on every commit, as production does.
func keepRealFsync(t *testing.T) {
	t.Helper()

	realFsyncTests.Store(t.Name(), true)
	t.Cleanup(func() { realFsyncTests.Delete(t.Name()) })
}

// usesRealFsync reports whether t or an ancestor called keepRealFsync.
func usesRealFsync(t *testing.T) bool {
	t.Helper()

	for name := t.Name(); name != ""; name = parentTestName(name) {
		if _, ok := realFsyncTests.Load(name); ok {
			return true
		}
	}

	return false
}

// storeSynchronous is the synchronous setting of eng's store connection.
func storeSynchronous(t *testing.T, eng *Engine) int {
	t.Helper()

	var synchronous int
	require.NoError(t, syncstoretest.RawDB(t, eng.baseline).QueryRowContext(t.Context(), "PRAGMA synchronous").Scan(&synchronous))

	return synchronous
}

// A test engine's store skips the waits for the disk, and one built under
// keepRealFsync, like a production engine's, does not.
//
// Validates: R-6.10.29
func TestTestEngines_SkipFsyncUnlessTheTestKeepsIt(t *testing.T) {
	t.Parallel()

	t.Run("default", func(t *testing.T) {
		t.Parallel()

		eng, _ := newTestEngine(t, &syncfixture.EngineMockClient{})
		assert.Equal(t, sqliteSynchronousOff, storeSynchronous(t, eng.Engine))
	})
	t.Run("kept", func(t *testing.T) {
		t.Parallel()

		keepRealFsync(t)
		eng, _ := newTestEngine(t, &syncfixture.EngineMockClient{})
		assert.Equal(t, sqliteSynchronousFull, storeSynchronous(t, eng.Engine))
	})
}
