package syncexec

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// Validates: R-2.10.1, R-2.10.3, R-2.10.66
//
// The completion records what the server answered when it is built, read
// from the error the outcome carries, so classification and scope detection
// read one recorded answer.
func TestCompletionFromTrackedAction_RecordsTheServerAnswer(t *testing.T) {
	t.Parallel()

	ta := &syncdomain.TrackedAction{ID: 7, Action: syncdomain.Action{
		Type: syncdomain.ActionUpload, Path: "a.txt", DriveID: driveid.New("0000000000000001"),
	}}
	throttled := &graph.GraphError{StatusCode: http.StatusTooManyRequests, RetryAfter: 30 * time.Second, Err: graph.ErrThrottled}
	quota := &graph.GraphError{StatusCode: http.StatusForbidden, Code: "quotaLimitReached", Err: graph.ErrForbidden}

	for name, tc := range map[string]struct {
		outcome    *syncdomain.ActionOutcome
		actionErr  error
		want       syncdomain.ServerAnswer
		wantStatus int
		wantRetry  time.Duration
	}{
		"success":            {outcome: &syncdomain.ActionOutcome{Action: syncdomain.ActionUpload, Success: true}, want: syncdomain.ServerAnswerNone},
		"outcome throttled":  {outcome: &syncdomain.ActionOutcome{Action: syncdomain.ActionUpload, Error: fmt.Errorf("upload: %w", throttled)}, want: syncdomain.ServerAnswerThrottled, wantStatus: 429, wantRetry: 30 * time.Second},
		"action error quota": {actionErr: quota, want: syncdomain.ServerAnswerQuota, wantStatus: 403},
		"local failure":      {actionErr: errors.New("disk on fire"), want: syncdomain.ServerAnswerNone},
	} {
		r := CompletionFromTrackedAction(ta, tc.outcome, tc.actionErr)
		assert.Equal(t, tc.want, r.ServerAnswer, name)
		assert.Equal(t, tc.wantStatus, r.HTTPStatus, name)
		assert.Equal(t, tc.wantRetry, r.RetryAfter, name)
	}
}

// Validates: R-2.10.1, R-2.10.3
//
// A test's hand-built completion carries the answer the constructor would
// record for the same error, and a status the test names wins.
func TestCompletionForTest_ReadsTheAnswerAsProductionDoes(t *testing.T) {
	t.Parallel()

	ge := &graph.GraphError{StatusCode: http.StatusServiceUnavailable, RetryAfter: time.Minute, Err: graph.ErrServerError}
	built := CompletionFromTrackedAction(&syncdomain.TrackedAction{}, nil, ge)
	byHand := CompletionForTest(&syncdomain.ActionCompletion{Err: ge})
	assert.Equal(t, built.ServerAnswer, byHand.ServerAnswer)
	assert.Equal(t, built.HTTPStatus, byHand.HTTPStatus)
	assert.Equal(t, built.RetryAfter, byHand.RetryAfter)

	named := CompletionForTest(&syncdomain.ActionCompletion{HTTPStatus: http.StatusTooManyRequests, Err: errors.New("x")})
	assert.Equal(t, syncdomain.ServerAnswerThrottled, named.ServerAnswer)
}

// Validates: R-2.10.1, R-2.10.3, R-2.10.65
func TestScopeKeyForResult(t *testing.T) {
	t.Parallel()

	key := func(status int, drive driveid.ID) syncdomain.ScopeKey {
		return syncdomain.ScopeKeyForResult(classifyServerAnswer(status, nil), drive)
	}
	assert.Equal(t, syncdomain.SKThrottleDrive(driveid.New("0000000000000001")), key(429, driveid.New("0000000000000001")))
	assert.True(t, key(429, driveid.ID{}).IsZero(), "429 without a drive id should be zero")
	assert.Equal(t, syncdomain.SKService(), key(503, driveid.ID{}))
	assert.Equal(t, syncdomain.SKService(), key(500, driveid.ID{}))
	assert.Equal(t, syncdomain.SKService(), key(502, driveid.ID{}))
	assert.Equal(t, syncdomain.SKQuotaOwn(), key(507, driveid.ID{}))
	assert.True(t, key(501, driveid.ID{}).IsZero(), "a refusal is not service evidence")
	assert.True(t, key(505, driveid.ID{}).IsZero(), "a refusal is not service evidence")
	assert.True(t, key(404, driveid.ID{}).IsZero(), "non-scope status should be zero")
	assert.True(t, key(200, driveid.ID{}).IsZero(), "success status should be zero")
}

func TestExtractRetryAfter_GraphError(t *testing.T) {
	t.Parallel()

	ge := &graph.GraphError{StatusCode: 429, RetryAfter: 30 * time.Second}
	assert.Equal(t, 30*time.Second, extractRetryAfter(ge))
}

func TestExtractRetryAfter_Nil(t *testing.T) {
	t.Parallel()

	assert.Equal(t, time.Duration(0), extractRetryAfter(nil))
}

// TestExtractRetryAfter_Wrapped verifies that extractRetryAfter works with wrapped errors.
func TestExtractRetryAfter_Wrapped(t *testing.T) {
	t.Parallel()

	ge := &graph.GraphError{StatusCode: 503, RetryAfter: 120 * time.Second}
	wrapped := fmt.Errorf("request failed: %w", ge)
	assert.Equal(t, 120*time.Second, extractRetryAfter(wrapped))
}
