package sync

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

// Validates: R-2.4.15
func TestExecutor_Download_RefusesToReplaceDatabaseWithLiveWAL(t *testing.T) {
	t.Parallel()

	for _, sidecar := range []string{"-wal", "-journal"} {
		t.Run(sidecar, func(t *testing.T) {
			t.Parallel()

			downloads := 0
			dl := &executorMockDownloader{
				downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
					downloads++
					n, err := w.Write([]byte("new"))
					return int64(n), err
				},
			}
			cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, dl, &executorMockUploader{})
			require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "app.db"), []byte("old"), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "app.db"+sidecar), []byte("pages"), 0o600))

			o := NewExecution(cfg, emptyBaseline()).ExecuteDownload(t.Context(), databaseDownloadAction())

			require.False(t, o.Success)
			require.ErrorIs(t, o.Error, ErrDatabaseInUse)
			assert.Zero(t, downloads, "nothing is fetched for an open database")
			data, err := localpath.ReadFile(filepath.Join(syncRoot, "app.db"))
			require.NoError(t, err)
			assert.Equal(t, "old", string(data))
			assert.Equal(t, IssueLocalDatabaseOpen, classifyResult(&ActionCompletion{Err: o.Error}).ConditionType)
		})
	}
}
