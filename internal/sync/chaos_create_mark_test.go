package sync

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// Validates: R-5.6.15
//
// Which create failures carry graph.ErrCreateOutcomeUnknown is written once
// in the chaos layer (chaosAnswer.createMarked). Here every answer the layer
// draws for a create is played to graph.CreateFile by a loopback server, so
// a change of graph's own rule fails this test instead of leaving the layer
// to mark what graph no longer marks. A create that never left the client is
// among them: it reaches no server and carries no mark.
func TestChaosCreateFault_MarksWhatTheGraphClientMarks(t *testing.T) {
	t.Parallel()

	answers := chaosCreateAnswers()
	require.True(t, slices.ContainsFunc(answers, func(answer chaosAnswer) bool { return answer.notSent }),
		"the create faults include a create that never left the client")
	for _, answer := range answers {
		t.Run(answer.kind(), func(t *testing.T) {
			t.Parallel()

			requests, graphErr := createThroughGraph(t, answer)
			require.Error(t, graphErr)
			assert.Equal(t, answer.createMarked, errors.Is(graphErr, graph.ErrCreateOutcomeUnknown),
				"graph marks %s: %v", answer.kind(), graphErr)
			chaosErr := chaosCreateFailure("doc.txt", answer, answer.failure(chaosOpCreate, "", time.Second))
			assert.Equal(t, answer.createMarked, errors.Is(chaosErr, graph.ErrCreateOutcomeUnknown),
				"the chaos layer marks %s: %v", answer.kind(), chaosErr)

			wantRequests := int64(1)
			if answer.notSent {
				wantRequests = 0
			}
			assert.Equal(t, wantRequests, requests, "requests that reached the server")
			if answer.status == 0 {
				return
			}
			var graphAnswer *graph.GraphError
			require.ErrorAs(t, graphErr, &graphAnswer)
			assert.Equal(t, answer.status, graphAnswer.StatusCode)
			if answer.sentinel != nil {
				require.ErrorIs(t, graphErr, answer.sentinel, "graph classifies the status as the layer does")
			}
		})
	}
}

// chaosPinRetryAfterSeconds is the Retry-After of an answer that carries one.
const chaosPinRetryAfterSeconds = "1"

// createThroughGraph runs graph.CreateFile against a loopback server that
// gives answer, and returns how many requests reached the server and the
// create's failure.
func createThroughGraph(t *testing.T, answer chaosAnswer) (int64, error) {
	t.Helper()

	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		if answer.status == 0 {
			dropConnection(t, w)
			return
		}
		if answer.retryAfter {
			w.Header().Set("Retry-After", chaosPinRetryAfterSeconds)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(answer.status)
		body := map[string]map[string]string{"error": {"code": answer.code, "message": "chaos"}}
		assert.NoError(t, json.NewEncoder(w).Encode(body))
	}))
	t.Cleanup(server.Close)

	var tokenErr error
	if answer.notSent {
		tokenErr = errChaosNotSent
	}
	client, err := graph.NewClient(server.URL, &http.Client{Timeout: eventWait}, chaosPinTokenSource{err: tokenErr},
		slog.New(slog.DiscardHandler), "chaos-pin")
	require.NoError(t, err)

	_, createErr := client.CreateFile(t.Context(), driveid.New(engineTestDriveID), graphRootID, "doc.txt")

	return requests.Load(), createErr
}

// dropConnection closes the request's connection without an answer, as a
// connection reset does.
func dropConnection(t *testing.T, w http.ResponseWriter) {
	t.Helper()

	hijacker, ok := w.(http.Hijacker)
	if !assert.True(t, ok, "the server cannot drop the connection") {
		return
	}
	conn, _, err := hijacker.Hijack()
	if !assert.NoError(t, err) {
		return
	}
	assert.NoError(t, conn.Close())
}

// chaosPinTokenSource hands graph a token, or fails as a token source whose
// sign-in cannot be read does.
type chaosPinTokenSource struct {
	err error
}

func (s chaosPinTokenSource) Token() (string, error) {
	if s.err != nil {
		return "", s.err
	}

	return "chaos-pin", nil
}
