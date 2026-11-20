package sync

import (
	"path/filepath"
	"testing"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncstoretest"
)

// newTestEngineOnFaultStore is newTestEngine with the engine's store opened
// through the fault driver, and the fault that governs its connections.
func newTestEngineOnFaultStore(
	t *testing.T, mock *syncfixture.EngineMockClient, configure func(*engineInputs),
) (*testEngine, *syncfixture.SqlFault) {
	t.Helper()

	syncstoretest.RegisterFaultDriver(t)
	tmpDir := t.TempDir()
	eng, _ := newTestEngineInDirWith(t, t.Context(), mock, tmpDir, func(inputs *engineInputs) {
		inputs.hooks.storeDriver = syncfixture.FaultDriverName
		if configure != nil {
			configure(inputs)
		}
	})

	return eng, syncfixture.FaultForPath(filepath.Join(tmpDir, "test.db"))
}
