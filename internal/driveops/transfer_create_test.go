package driveops

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// tmCreatingUploader adds UploadCreating and DeleteItemIfMatch to
// tmMockUploader, recording what a create did.
type tmCreatingUploader struct {
	tmMockUploader
	created        *graph.Item
	contentErr     error
	contentResult  *graph.Item
	events         []string
	deletedIfMatch []string
}

func (m *tmCreatingUploader) UploadCreating(
	_ context.Context, _ driveid.ID, _, _ string,
	_ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc, onCreated graph.CreatedFunc,
) (*graph.Item, error) {
	m.events = append(m.events, "create")
	if onCreated != nil {
		if err := onCreated(m.created); err != nil {
			return nil, &graph.CreatedItemError{Item: m.created, Err: err}
		}
	}
	m.events = append(m.events, "content")
	if m.contentErr != nil {
		return nil, &graph.CreatedItemError{Item: m.created, Err: m.contentErr}
	}

	return m.contentResult, nil
}

func (m *tmCreatingUploader) DeleteItemIfMatch(_ context.Context, _ driveid.ID, itemID, ifMatch string) error {
	m.deletedIfMatch = append(m.deletedIfMatch, itemID+"@"+ifMatch)
	return nil
}

func writeCreateTestFile(t *testing.T, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "new.txt")
	require.NoError(t, os.WriteFile(p, data, 0o600))
	return p
}

// Validates: R-5.6.15
func TestUploadFile_RecordsTheCreatedItemBeforeAnyContent(t *testing.T) {
	t.Parallel()

	data := []byte("content")
	ul := &tmCreatingUploader{
		created:       &graph.Item{ID: "new-item", ETag: `"c,1"`},
		contentResult: &graph.Item{ID: "new-item", Size: int64(len(data)), QuickXorHash: tmHashBytes(data)},
	}
	tm := newTestTM(&tmSimpleDownloader{}, ul, nil)

	_, err := tm.UploadFile(t.Context(), driveid.New("d1"), "parent", "new.txt", writeCreateTestFile(t, data), UploadOpts{
		OnCreated: func(itemID, createdETag string) error {
			ul.events = append(ul.events, "recorded "+itemID+" "+createdETag)
			return nil
		},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"create", `recorded new-item "c,1"`, "content"}, ul.events)
}

// Validates: R-5.6.15
func TestUploadFile_UncertainFinalFragmentThatDidNotLandRemovesThePlaceholder(t *testing.T) {
	t.Parallel()

	data := []byte("content")
	var lookedUp []string
	ul := &tmCreatingUploader{
		created:    &graph.Item{ID: "new-item", ETag: `"c,1"`},
		contentErr: &graph.FinalFragmentError{Err: errors.New("connection reset")},
		tmMockUploader: tmMockUploader{
			getItemFn: func(_ context.Context, _ driveid.ID, itemID string) (*graph.Item, error) {
				lookedUp = append(lookedUp, itemID)
				// Still the empty placeholder: the content did not land.
				return &graph.Item{ID: itemID, ETag: `"c,1"`, QuickXorHash: tmHashBytes(nil)}, nil
			},
			listChildrenFn: func(context.Context, driveid.ID, string) ([]graph.Item, error) {
				return nil, errors.New("a create's outcome must be looked up by ID, not by name")
			},
		},
	}
	tm := newTestTM(&tmSimpleDownloader{}, ul, nil)

	_, err := tm.UploadFile(t.Context(), driveid.New("d1"), "parent", "new.txt", writeCreateTestFile(t, data), UploadOpts{
		OnCreated: func(string, string) error { return nil },
	})
	require.Error(t, err)
	assert.Equal(t, []string{"new-item"}, lookedUp)
	assert.Equal(t, []string{`new-item@"c,1"`}, ul.deletedIfMatch)
}

// Validates: R-5.6.15
func TestSessionUpload_RetryOfAnInterruptedCreateReusesItsItem(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewSessionStore(dir, slog.Default())
	localPath, data, hash := sessionTestFile(t, dir)
	// A previous run created the item and recorded the session, then died;
	// the session is gone now.
	require.NoError(t, store.Save(driveid.New("d1").String(), localPath, &SessionRecord{
		SessionURL: "https://upload.example.com/old", FileHash: hash, FileSize: int64(len(data)),
		ExpiresAt: time.Now().Add(-time.Hour), ItemID: "placeholder", CreatedETag: `"p,1"`,
	}))

	ul := &tmItemSessionUploader{tmMockUploader: tmMockUploader{
		resumeUploadFn: func(context.Context, *graph.UploadSession, io.ReaderAt, int64, graph.ProgressFunc) (*graph.Item, error) {
			return nil, graph.ErrUploadSessionExpired
		},
		getItemFn: func(_ context.Context, _ driveid.ID, itemID string) (*graph.Item, error) {
			return &graph.Item{ID: itemID, ETag: `"p,1"`}, nil
		},
		createUploadSessionFn: func(context.Context, driveid.ID, string, string, int64, time.Time) (*graph.UploadSession, error) {
			return nil, errors.New("must not create the file again: it would 409 against its own placeholder")
		},
		uploadFromSessionFn: func(_ context.Context, _ *graph.UploadSession, _ io.ReaderAt, _ int64, _ graph.ProgressFunc) (*graph.Item, error) {
			return &graph.Item{ID: "placeholder", Size: int64(len(data)), QuickXorHash: hash}, nil
		},
	}}
	tm := newTestTM(&tmSimpleDownloader{}, ul, store)

	item, err := tm.UploadFile(t.Context(), driveid.New("d1"), "parent", filepath.Base(localPath), localPath, UploadOpts{})
	require.NoError(t, err)
	assert.Equal(t, "placeholder", item.Item.ID)
	assert.Equal(t, []string{`"p,1"`}, ul.ifMatches, "the fresh session goes to the existing item, conditionally")
}
