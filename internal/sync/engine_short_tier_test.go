package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
)

// Validates: R-6.10.22
func TestShortTier_TheEngineHarnessRunsEveryTargetedTest(t *testing.T) {
	t.Parallel()

	// The per-PR lane is the short tier. A harness that skipped engine tests
	// under -short by sample let targeted safety tests reach main untested
	// on macOS (the 2026-11-23 nightly), so building a test engine must never
	// skip; only a test marked exhaustive skips itself.
	built := false
	t.Run("build", func(t *testing.T) {
		newTestEngineInputs(t, &syncfixture.EngineMockClient{}, t.TempDir())
		built = true
	})

	assert.True(t, built, "the engine test harness skipped a test")
}
