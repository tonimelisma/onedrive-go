package driveops

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// unknownOutcomeCase is an upload whose final fragment was answered with an
// error that may hide a commit, followed by a lookup of the item that fails
// on its own.
type unknownOutcomeCase struct {
	name   string
	answer *graph.GraphError
	lookup *graph.GraphError
}

// unknownOutcomeCases crosses the answers sync reads as "remote truth moved
// on" (409, 412) with lookups that fail with an outage or a throttle.
func unknownOutcomeCases() []unknownOutcomeCase {
	var cases []unknownOutcomeCase
	for _, answer := range []*graph.GraphError{
		{StatusCode: http.StatusConflict, Code: "nameAlreadyExists", Err: graph.ErrConflict},
		{StatusCode: http.StatusPreconditionFailed, Code: "resourceModified", Err: graph.ErrPreconditionFailed},
	} {
		for _, lookup := range []*graph.GraphError{
			{StatusCode: http.StatusServiceUnavailable, Err: graph.ErrServerError},
			{StatusCode: http.StatusTooManyRequests, Err: graph.ErrThrottled},
		} {
			cases = append(cases, unknownOutcomeCase{
				name:   fmt.Sprintf("%d then a %d lookup", answer.StatusCode, lookup.StatusCode),
				answer: answer,
				lookup: lookup,
			})
		}
	}

	return cases
}

// finalFragmentAnswer is how the upload of tc's final fragment fails.
func (tc unknownOutcomeCase) finalFragmentAnswer() error {
	return fmt.Errorf("upload: %w", &graph.FinalFragmentError{Err: tc.answer})
}

// requireOutcomeUnknownOfTheLookup checks that err says the upload's outcome
// is unknown and carries only the lookup's failure: the answer before it is
// named in the text, but neither its sentinel nor its status is in the chain.
func requireOutcomeUnknownOfTheLookup(t *testing.T, tc unknownOutcomeCase, err error) {
	t.Helper()

	require.ErrorIs(t, err, tc.lookup)
	require.NotErrorIs(t, err, graph.ErrConflict)
	require.NotErrorIs(t, err, graph.ErrPreconditionFailed)
	assert.False(t, graph.IsFinalFragmentUncertain(err), "the uncertain answer was settled by the lookup")
	var graphErr *graph.GraphError
	require.ErrorAs(t, err, &graphErr)
	assert.Equal(t, tc.lookup.StatusCode, graphErr.StatusCode, "the lookup's status, not the answer's")
	require.ErrorContains(t, err, "outcome is unknown")
	require.ErrorContains(t, err, "HTTP "+strconv.Itoa(tc.answer.StatusCode), "the answer is named in the text")
}

// Validates: R-5.6.12
//
// A small file's final fragment answered 409 or 412, and the lookup of what
// landed then failed (settleUncertainFinalFragment). Joined to the answer, the
// lookup's failure read as a stale plan in sync and its outage or throttle was
// hidden behind the answer's status.
func TestUploadFile_FailedLookupAfterAnUncertainFinalFragmentIsAnUnknownOutcome(t *testing.T) {
	t.Parallel()

	for _, tc := range unknownOutcomeCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ul := &tmMockUploader{
				uploadFn: func(context.Context, driveid.ID, string, string, io.ReaderAt, int64, time.Time, graph.ProgressFunc) (*graph.Item, error) {
					return nil, tc.finalFragmentAnswer()
				},
				listChildrenFn: func(context.Context, driveid.ID, string) ([]graph.Item, error) {
					return nil, tc.lookup
				},
			}
			localPath := filepath.Join(t.TempDir(), "a.txt")
			require.NoError(t, os.WriteFile(localPath, []byte("small file"), 0o600))

			_, err := newTestTM(&tmSimpleDownloader{}, ul, nil).UploadFile(
				t.Context(), driveid.New("d1"), "parent1", "a.txt", localPath, UploadOpts{})

			requireOutcomeUnknownOfTheLookup(t, tc, err)
		})
	}
}

// Validates: R-5.6.12
//
// A large file's final fragment through a fresh session answered 409 or 412,
// and the lookup failed. The outcome is unknown, so the session record stays
// for the next attempt to resume or look again, and the error is the lookup's.
func TestSessionUpload_FailedLookupAfterAnUncertainFinalFragmentIsAnUnknownOutcome(t *testing.T) {
	t.Parallel()

	for _, tc := range unknownOutcomeCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			store := NewSessionStore(dir, slog.Default())
			localPath, _, _ := sessionTestFile(t, dir)
			ul := &tmMockUploader{
				createUploadSessionFn: func(context.Context, driveid.ID, string, string, int64, time.Time) (*graph.UploadSession, error) {
					return &graph.UploadSession{UploadURL: "https://upload.example.com/s1", ExpirationTime: time.Now().Add(time.Hour)}, nil
				},
				uploadFromSessionFn: func(context.Context, *graph.UploadSession, io.ReaderAt, int64, graph.ProgressFunc) (*graph.Item, error) {
					return nil, tc.finalFragmentAnswer()
				},
				listChildrenFn: func(context.Context, driveid.ID, string) ([]graph.Item, error) {
					return nil, tc.lookup
				},
			}

			_, err := newTestTM(&tmSimpleDownloader{}, ul, store).UploadFile(
				t.Context(), driveid.New("d1"), "parent1", "big.bin", localPath, UploadOpts{})

			requireOutcomeUnknownOfTheLookup(t, tc, err)
			_, found, loadErr := store.Load(driveid.New("d1").String(), localPath)
			require.NoError(t, loadErr)
			assert.True(t, found, "the record is kept for the next attempt")
		})
	}
}

// Validates: R-5.6.12
//
// A resumed session's final fragment answered 409 or 412, and the lookup
// failed: the same unknown outcome, with the record kept and no fresh session.
func TestSessionUpload_FailedLookupAfterAnUncertainResumeIsAnUnknownOutcome(t *testing.T) {
	t.Parallel()

	for _, tc := range unknownOutcomeCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tm, ul, store, localPath, creates := adoptionFixture(t, &graph.Item{}, tc.lookup)
			ul.resumeUploadFn = func(context.Context, *graph.UploadSession, io.ReaderAt, int64, graph.ProgressFunc) (*graph.Item, error) {
				return nil, tc.finalFragmentAnswer()
			}

			_, err := tm.UploadFileToItem(t.Context(), driveid.New("d1"), "item-1", localPath,
				UploadOpts{Mtime: adoptionMtime(), PlannedRemoteETag: "planned"})

			requireOutcomeUnknownOfTheLookup(t, tc, err)
			assert.Zero(t, *creates, "no fresh session")
			_, found, loadErr := store.Load(driveid.New("d1").String(), existingItemSessionKey(localPath, "item-1"))
			require.NoError(t, loadErr)
			assert.True(t, found, "the record is kept for the next attempt")
		})
	}
}
