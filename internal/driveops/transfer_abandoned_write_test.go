package driveops

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/tonimelisma/onedrive-go/internal/synctree"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

// Validates: R-6.8.20, R-6.8.32
//
// A download whose write the graph client abandoned on a hung disk leaves
// its partial to the stuck write: the manager neither closes nor removes it,
// and the next attempt neither resumes it nor truncates it, but writes a
// single-use partial of another name, so the stuck write's bytes, landing
// whenever it returns, cannot end up inside the retry's file. Once the stuck
// write has returned, its partial is removed and the file's own partial
// name is used again.
func TestDownloadToFile_AnAbandonedWriteDisownsItsPartial(t *testing.T) {
	t.Parallel()

	const content = "the whole file"
	var sinks []graph.AbandonableWriter
	dl := &tmMockDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			sink, ok := w.(graph.AbandonableWriter)
			require.True(t, ok, "a download's partial can be abandoned")
			sinks = append(sinks, sink)

			if len(sinks) == 1 {
				n, err := w.Write([]byte("first"))
				require.NoError(t, err)
				// The next write hangs, and the graph client abandons it.
				sink.Abandon()

				return int64(n), fmt.Errorf("download: %w: %w: %w",
					graph.ErrTransferStalled, graph.ErrLocalSideStalled, graph.ErrLocalCallAbandoned)
			}

			n, err := w.Write([]byte(content))

			return int64(n), err
		},
		downloadRangeFn: func(context.Context, driveid.ID, string, io.Writer, int64) (int64, error) {
			return 0, errors.New("resumed into a partial a stuck write still holds")
		},
	}
	tm := newTestTM(dl, &tmMockUploader{}, nil)

	targetPath := filepath.Join(t.TempDir(), "big.bin")
	opts := DownloadOpts{RemoteETag: "v1", RemoteSize: int64(len(content))}
	ownPartial := DownloadPartialPath(targetPath, opts)

	_, err := tm.DownloadToFile(t.Context(), driveid.New("d1"), "item1", targetPath, opts)
	require.ErrorIs(t, err, graph.ErrTransferStalled)
	require.ErrorIs(t, err, graph.ErrLocalSideStalled)
	require.ErrorIs(t, err, graph.ErrLocalCallAbandoned, "sync reads the call left behind from the error")
	require.FileExists(t, ownPartial, "the stuck write owns the partial; the manager leaves it")
	_, err = sinks[0].Write([]byte("more"))
	require.ErrorIs(t, err, errPartialDisowned, "nothing but the stuck write may write a disowned partial")

	// The retry, while the write is still stuck.
	result, err := tm.DownloadToFile(t.Context(), driveid.New("d1"), "item1", targetPath, opts)
	require.NoError(t, err)
	assert.Equal(t, int64(len(content)), result.Size)
	require.Len(t, sinks, 2)
	assert.NotEqual(t, ownPartial, sinks[1].Name(), "the retry wrote the partial the stuck write holds")
	assert.True(t, synctree.IsOwnedTransferArtifactName(filepath.Base(sinks[1].Name())),
		"a single-use partial is still an owned one, so a leftover is cleaned up")
	assert.NoFileExists(t, sinks[1].Name(), "the single-use partial became the file")

	// The stuck write lands at last, in the disowned partial, and returns.
	stuck, err := localpath.OpenFile(ownPartial, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = stuck.WriteString(" late bytes")
	require.NoError(t, err)
	require.NoError(t, stuck.Close())
	sinks[0].AbandonedWriteReturned()

	got, err := localpath.ReadFile(targetPath)
	require.NoError(t, err)
	assert.Equal(t, content, string(got), "the stuck write's bytes stayed out of the file")
	assert.NoFileExists(t, ownPartial, "the disowned partial is removed once its write returned")

	// The file's own partial name is free again.
	_, err = tm.DownloadToFile(t.Context(), driveid.New("d1"), "item1", targetPath, opts)
	require.NoError(t, err)
	require.Len(t, sinks, 3)
	assert.Equal(t, ownPartial, sinks[2].Name())
}

// Validates: R-6.8.20
//
// A single-use partial is never resumed, so an attempt on one that fails is
// not kept for a resume, even when it stalled: it is removed.
func TestDownloadToFile_AFailedSingleUsePartialIsRemoved(t *testing.T) {
	t.Parallel()

	var sinks []graph.AbandonableWriter
	dl := &tmMockDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			sink, ok := w.(graph.AbandonableWriter)
			require.True(t, ok)
			sinks = append(sinks, sink)
			n, err := w.Write([]byte("half"))
			require.NoError(t, err)
			if len(sinks) == 1 {
				sink.Abandon()
			}

			return int64(n), fmt.Errorf("download: %w", graph.ErrTransferStalled)
		},
	}
	tm := newTestTM(dl, &tmMockUploader{}, nil)

	targetPath := filepath.Join(t.TempDir(), "big.bin")
	opts := DownloadOpts{RemoteETag: "v1", RemoteSize: 100}

	_, err := tm.DownloadToFile(t.Context(), driveid.New("d1"), "item1", targetPath, opts)
	require.ErrorIs(t, err, graph.ErrTransferStalled)
	_, err = tm.DownloadToFile(t.Context(), driveid.New("d1"), "item1", targetPath, opts)
	require.ErrorIs(t, err, graph.ErrTransferStalled)

	require.Len(t, sinks, 2)
	assert.NoFileExists(t, sinks[1].Name(), "a stalled single-use partial was kept, though nothing resumes it")
	assert.FileExists(t, DownloadPartialPath(targetPath, opts), "the stuck write's partial is its own to remove")
	sinks[0].AbandonedWriteReturned()
	assert.NoFileExists(t, DownloadPartialPath(targetPath, opts))
}
