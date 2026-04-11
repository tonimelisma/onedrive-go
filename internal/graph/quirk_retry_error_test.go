package graph

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/retry"
)

func TestDoQuirkRetry_ExhaustionWrapsTerminalErrorWithAttemptEvidence(t *testing.T) {
	t.Parallel()

	var logBuf bytes.Buffer
	client := newTestClient(t, "http://unused")
	client.logger = slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	var attemptCount int
	_, err := doQuirkRetry[string](t.Context(), client, quirkRetrySpec{
		name: "drives-token-propagation",
		policy: retry.Policy{
			MaxAttempts: 2,
			Base:        0,
			Max:         0,
			Multiplier:  1,
			Jitter:      0,
		},
		match: isTransientDrivesDiscoveryError,
	}, func() (string, error) {
		attemptCount++

		return "", &GraphError{
			StatusCode: http.StatusForbidden,
			RequestID:  fmt.Sprintf("req-%d", attemptCount),
			Code:       "accessDenied",
			Message:    "Access denied",
			Err:        ErrForbidden,
		}
	})

	require.Error(t, err)
	require.ErrorIs(t, err, ErrForbidden)

	var graphErr *GraphError
	require.ErrorAs(t, err, &graphErr)
	assert.Equal(t, "req-2", graphErr.RequestID)

	var quirkErr *QuirkRetryError
	require.ErrorAs(t, err, &quirkErr)
	assert.Equal(t, "drives-token-propagation", quirkErr.Quirk)
	require.Len(t, quirkErr.Attempts, 2)
	assert.Equal(t, 1, quirkErr.Attempts[0].Attempt)
	assert.Equal(t, http.StatusForbidden, quirkErr.Attempts[0].StatusCode)
	assert.Equal(t, "accessDenied", quirkErr.Attempts[0].GraphCode)
	assert.Equal(t, "req-1", quirkErr.Attempts[0].RequestID)
	assert.Equal(t, 2, quirkErr.Attempts[1].Attempt)
	assert.Equal(t, "req-2", quirkErr.Attempts[1].RequestID)
	assert.Contains(t, quirkErr.Error(), "retry exhausted")

	logs := decodeJSONLogLines(t, logBuf.Bytes())
	require.Len(t, logs, 2)
	assert.Equal(t, "retrying after documented graph quirk", logs[0]["msg"])
	assert.Equal(t, "graph quirk retry exhausted", logs[1]["msg"])
	assert.Equal(t, "drives-token-propagation", logs[1]["graph_quirk"])
	assert.EqualValues(t, 2, logs[1]["graph_quirk_attempt_count"])
	assert.Contains(t, logBuf.String(), "\"graphCode\":\"accessDenied\"")
	assert.Contains(t, logBuf.String(), "\"requestId\":\"req-1\"")
}

func TestDoQuirkRetry_NonRetryableFailureDoesNotWrap(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, "http://unused")
	want := &GraphError{
		StatusCode: http.StatusForbidden,
		RequestID:  "req-final",
		Code:       "differentError",
		Message:    "Forbidden",
		Err:        ErrForbidden,
	}

	_, err := doQuirkRetry[string](t.Context(), client, quirkRetrySpec{
		name: "drives-token-propagation",
		policy: retry.Policy{
			MaxAttempts: 2,
			Base:        0,
			Max:         0,
			Multiplier:  1,
			Jitter:      0,
		},
		match: isTransientDrivesDiscoveryError,
	}, func() (string, error) {
		return "", want
	})

	require.Error(t, err)
	assert.Same(t, want, err)

	var quirkErr *QuirkRetryError
	assert.NotErrorAs(t, err, &quirkErr)
}

func decodeJSONLogLines(t *testing.T, raw []byte) []map[string]any {
	t.Helper()

	lines := bytes.Split(bytes.TrimSpace(raw), []byte{'\n'})
	records := make([]map[string]any, 0, len(lines))
	for i := range lines {
		if len(bytes.TrimSpace(lines[i])) == 0 {
			continue
		}

		var record map[string]any
		err := json.Unmarshal(lines[i], &record)
		require.NoError(t, err)
		records = append(records, record)
	}

	return records
}

func TestQuirkRetryError_UnwrapsTerminalError(t *testing.T) {
	t.Parallel()

	err := &QuirkRetryError{
		Quirk: "download-metadata-transient-404",
		Attempts: []QuirkRetryAttempt{
			{
				Attempt:    1,
				StatusCode: http.StatusNotFound,
				GraphCode:  "itemNotFound",
				RequestID:  "req-1",
			},
		},
		Err: &GraphError{
			StatusCode: http.StatusNotFound,
			RequestID:  "req-1",
			Code:       "itemNotFound",
			Message:    "Not found",
			Err:        ErrNotFound,
			RetryAfter: time.Second,
		},
	}

	require.ErrorIs(t, err, ErrNotFound)

	var graphErr *GraphError
	require.ErrorAs(t, err, &graphErr)
	assert.Equal(t, "req-1", graphErr.RequestID)
	assert.Equal(t, time.Second, graphErr.RetryAfter)
}
