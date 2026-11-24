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
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncexec"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// Validates: R-2.4.15
func TestExecutor_Download_RefusesToReplaceDatabaseWithLiveWAL(t *testing.T) {
	t.Parallel()

	for _, sidecar := range []string{"-wal", "-journal"} {
		t.Run(sidecar, func(t *testing.T) {
			t.Parallel()

			downloads := 0
			dl := &syncfixture.ExecutorMockDownloader{
				DownloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
					downloads++
					n, err := w.Write([]byte("new"))
					return int64(n), err
				},
			}
			cfg, syncRoot := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, dl, &syncfixture.ExecutorMockUploader{})
			require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "app.db"), []byte("old"), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "app.db"+sidecar), []byte("pages"), 0o600))

			o := syncexec.NewExecution(cfg, syncfixture.EmptyBaseline()).ExecuteDownload(t.Context(), syncfixture.DatabaseDownloadAction())

			require.False(t, o.Success)
			require.ErrorIs(t, o.Error, syncexec.ErrDatabaseInUse)
			assert.Zero(t, downloads, "nothing is fetched for an open database")
			data, err := localpath.ReadFile(filepath.Join(syncRoot, "app.db"))
			require.NoError(t, err)
			assert.Equal(t, "old", string(data))
			assert.Equal(t, syncdomain.IssueLocalDatabaseOpen, classifyResult(syncexec.CompletionForTest(&syncdomain.ActionCompletion{Err: o.Error})).ConditionType)
		})
	}
}
