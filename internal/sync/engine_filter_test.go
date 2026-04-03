package sync

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// Validates: R-2.4.1, R-2.4.2, R-2.4.3
func TestRunOnce_LocalFilterConfigSuppressesConfiguredUploads(t *testing.T) {
	t.Parallel()

	var uploaded []string
	mock := &engineMockClient{
		uploadFn: func(
			_ context.Context,
			_ driveid.ID,
			_ string,
			name string,
			_ io.ReaderAt,
			_ int64,
			_ time.Time,
			_ graph.ProgressFunc,
		) (*graph.Item, error) {
			uploaded = append(uploaded, name)
			return &graph.Item{
				ID:           "uploaded-" + name,
				Name:         name,
				QuickXorHash: "hash-" + name,
			}, nil
		},
	}

	eng, syncRoot := newTestEngine(t, mock)
	eng.localFilter = synctypes.LocalFilterConfig{
		SkipDotfiles: true,
		SkipDirs:     []string{"vendor"},
		SkipFiles:    []string{"*.log"},
	}

	writeLocalFile(t, syncRoot, ".env", "secret")
	writeLocalFile(t, syncRoot, "vendor/lib.txt", "vendored")
	writeLocalFile(t, syncRoot, "debug.log", "noise")
	writeLocalFile(t, syncRoot, "keep.txt", "keep")

	report, err := eng.RunOnce(t.Context(), synctypes.SyncUploadOnly, synctypes.RunOpts{})
	require.NoError(t, err)

	assert.Equal(t, []string{"keep.txt"}, uploaded)
	assert.Equal(t, 1, report.Uploads)

	issues, issueErr := eng.baseline.ListSyncFailures(t.Context())
	require.NoError(t, issueErr)
	assert.Empty(t, issues, "configured exclusions should not record actionable failures")
}
