package syncexec

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// An executor configuration built with zero hooks behaves as production
// does: QuickXorHash for files and open descriptors, the full store-access
// and outcome-commit bounds, and crash points that do nothing.
//
// Validates: R-6.10.29
func TestHooks_ZeroValueIsProduction(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{},
		&syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	content := []byte("hooked or not\n")
	path := filepath.Join(syncRoot, "file.txt")
	require.NoError(t, os.WriteFile(path, content, 0o600))

	want, err := driveops.ComputeQuickXorHashContext(t.Context(), path)
	require.NoError(t, err)
	byPath, err := cfg.hashFile(t.Context(), path)
	require.NoError(t, err)
	assert.Equal(t, want, byPath, "a file hashes with QuickXorHash")
	byReader, err := cfg.hashOpenFile(t.Context(), bytes.NewReader(content))
	require.NoError(t, err)
	assert.Equal(t, want, byReader, "an open descriptor hashes with QuickXorHash")

	assert.Equal(t, syncdomain.OutcomeCommitTimeout, cfg.detachedStoreBound(), "store access keeps the full bound")
	pool := NewWorkerPool(cfg, nil, nil, cfg.logger, 1)
	assert.Equal(t, syncdomain.OutcomeCommitTimeout, pool.outcomeCommitBound(), "outcome commits keep the full bound")
	for _, point := range syncfixture.AllCrashPoints() {
		require.NoError(t, cfg.reachCrashPoint(point), "crash point %s does nothing", point)
	}
}
