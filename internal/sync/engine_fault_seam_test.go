package sync

import (
	"path/filepath"
	"testing"
)

// newTestEngineOnFaultStore is newTestEngine with the engine's store opened
// through the fault driver, and the fault that governs its connections.
func newTestEngineOnFaultStore(
	t *testing.T, mock *engineMockClient, configure func(*engineInputs),
) (*testEngine, *sqlFault) {
	t.Helper()

	registerFaultDriver(t)
	tmpDir := t.TempDir()
	eng, _ := newTestEngineInDirWith(t, t.Context(), mock, tmpDir, func(inputs *engineInputs) {
		inputs.storeDriver = faultDriverName
		if configure != nil {
			configure(inputs)
		}
	})

	return eng, faultForPath(filepath.Join(tmpDir, "test.db"))
}
