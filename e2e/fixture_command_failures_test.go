package e2e

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// Fixture recurrence reasons are recorder-owned taxonomy values because both
// command-window retries and later readiness waits emit the same quirk summary
// vocabulary.
type liveProviderRecurrenceReason string

const (
	liveProviderRecurrenceFreshParentChildCreateLag      liveProviderRecurrenceReason = "fresh_parent_child_create_lag"
	liveProviderRecurrenceFreshParentParentPathLag       liveProviderRecurrenceReason = "fresh_parent_parent_path_lag"
	liveProviderRecurrencePostMutationDestinationPathLag liveProviderRecurrenceReason = "post_mutation_destination_visibility_lag"
	liveProviderRecurrenceTransientGraphGatewayFailure   liveProviderRecurrenceReason = "transient_graph_gateway_failure"
	liveProviderRecurrenceUnknown                        liveProviderRecurrenceReason = "unknown"
)

type liveProviderRecurrenceDecision struct {
	Reason liveProviderRecurrenceReason
	Retry  bool
}

func isRetryableGraphGatewayFailure(stderr string) bool {
	return strings.Contains(stderr, "graph: HTTP 502") ||
		strings.Contains(stderr, "graph: HTTP 503") ||
		strings.Contains(stderr, "graph: HTTP 504")
}

func classifyFixturePutCommandFailure(stderr string) liveProviderRecurrenceDecision {
	switch {
	case strings.Contains(stderr, "probe account identity") &&
		isRetryableGraphGatewayFailure(stderr):
		return liveProviderRecurrenceDecision{
			Reason: liveProviderRecurrenceTransientGraphGatewayFailure,
			Retry:  true,
		}
	case graphQuirkExhausted(stderr, graph.QuirkFileCreateTransient404):
		return liveProviderRecurrenceDecision{
			Reason: liveProviderRecurrenceFreshParentChildCreateLag,
			Retry:  true,
		}
	case strings.Contains(stderr, "resolving parent") &&
		strings.Contains(stderr, "remote path not yet visible"):
		return liveProviderRecurrenceDecision{
			Reason: liveProviderRecurrenceFreshParentParentPathLag,
			Retry:  true,
		}
	default:
		return liveProviderRecurrenceDecision{
			Reason: liveProviderRecurrenceUnknown,
			Retry:  false,
		}
	}
}

// graphQuirkExhausted reports whether a --debug CLI run's stderr carries the
// structured "retry exhausted" record for quirk. It keys on the record's
// graph.QuirkLogKey field (text or JSON console form), not on error wording,
// so rewording the user-facing error cannot silently stop the classifier
// from recognizing the quirk.
func graphQuirkExhausted(stderr string, quirk string) bool {
	return strings.Contains(stderr, graph.QuirkLogKey+"="+quirk) ||
		strings.Contains(stderr, `"`+graph.QuirkLogKey+`":"`+quirk+`"`)
}

func classifyFixtureMkdirCommandFailure(stderr string) liveProviderRecurrenceDecision {
	switch {
	case strings.Contains(stderr, "probe account identity") &&
		isRetryableGraphGatewayFailure(stderr):
		return liveProviderRecurrenceDecision{
			Reason: liveProviderRecurrenceTransientGraphGatewayFailure,
			Retry:  true,
		}
	case strings.Contains(stderr, "remote path not yet visible"):
		return liveProviderRecurrenceDecision{
			Reason: liveProviderRecurrenceFreshParentParentPathLag,
			Retry:  true,
		}
	default:
		return liveProviderRecurrenceDecision{
			Reason: liveProviderRecurrenceUnknown,
			Retry:  false,
		}
	}
}

func TestClassifyFixturePutCommandFailure_KnownConvergenceFamilies(t *testing.T) {
	t.Parallel()

	assert.Equal(t, liveProviderRecurrenceDecision{
		Reason: liveProviderRecurrenceFreshParentChildCreateLag,
		Retry:  true,
	}, classifyFixturePutCommandFailure(
		`{"level":"DEBUG","msg":"graph quirk retry exhausted","graph_quirk":"file-create-transient-404","graph_quirk_attempt_count":6}`,
	))
	assert.Equal(t, liveProviderRecurrenceDecision{
		Reason: liveProviderRecurrenceFreshParentParentPathLag,
		Retry:  true,
	}, classifyFixturePutCommandFailure(
		`Error: resolving parent "e2e-fast-conflict": remote path not yet visible: "e2e-fast-conflict"`,
	))
	assert.Equal(t, liveProviderRecurrenceDecision{
		Reason: liveProviderRecurrenceTransientGraphGatewayFailure,
		Retry:  true,
	}, classifyFixturePutCommandFailure(
		`Error: probe account identity: fetch authenticated user for personal:user@example.com: graph: HTTP 504 (request-id: 50f613df-8467-4734-a771-524f70af34cc): ProfileException`,
	))
}

func TestClassifyFixturePutCommandFailure_RejectsUnrelatedFailures(t *testing.T) {
	t.Parallel()

	assert.Equal(t, liveProviderRecurrenceDecision{
		Reason: liveProviderRecurrenceUnknown,
		Retry:  false,
	}, classifyFixturePutCommandFailure(
		`time=2026-04-10T23:56:11.987-07:00 level=DEBUG msg="graph quirk retry exhausted" graph_quirk=simple-upload-mtime-transient-404 graph_quirk_attempt_count=4
Error: graph: setting mtime after simple upload: graph: OneDrive kept reporting the just-uploaded file as missing while setting its modified time, even after 4 tries`,
	))
	assert.Equal(t, liveProviderRecurrenceDecision{
		Reason: liveProviderRecurrenceUnknown,
		Retry:  false,
	}, classifyFixturePutCommandFailure(
		`Error: graph: HTTP 403: Access denied`,
	))
	assert.Equal(t, liveProviderRecurrenceDecision{
		Reason: liveProviderRecurrenceUnknown,
		Retry:  false,
	}, classifyFixturePutCommandFailure(
		`Error: resolving "/e2e-fast-conflict": resolve "e2e-fast-conflict" from the listing of parent "": graph: not found`,
	))
	assert.Equal(t, liveProviderRecurrenceDecision{
		Reason: liveProviderRecurrenceUnknown,
		Retry:  false,
	}, classifyFixturePutCommandFailure(
		`pollRemoteEventually: timed out after 30s waiting for remote fixture seed visibility for "test.txt" via [stat /folder/test.txt]`,
	))
}

func TestClassifyFixtureMkdirCommandFailure_KnownConvergenceFamilies(t *testing.T) {
	t.Parallel()

	assert.Equal(t, liveProviderRecurrenceDecision{
		Reason: liveProviderRecurrenceFreshParentParentPathLag,
		Retry:  true,
	}, classifyFixtureMkdirCommandFailure(
		`Error: confirming folder "e2e-sync-bidi-123/data" visibility: remote path not yet visible: "e2e-sync-bidi-123/data"`,
	))
	assert.Equal(t, liveProviderRecurrenceDecision{
		Reason: liveProviderRecurrenceTransientGraphGatewayFailure,
		Retry:  true,
	}, classifyFixtureMkdirCommandFailure(
		`Error: probe account identity: fetch authenticated user for personal:user@example.com: graph: HTTP 504 (request-id: 50f613df-8467-4734-a771-524f70af34cc): ProfileException`,
	))
}

func TestClassifyFixtureMkdirCommandFailure_RejectsUnrelatedFailures(t *testing.T) {
	t.Parallel()

	assert.Equal(t, liveProviderRecurrenceDecision{
		Reason: liveProviderRecurrenceUnknown,
		Retry:  false,
	}, classifyFixtureMkdirCommandFailure(
		`Error: creating folder "data": graph: HTTP 403: Access denied`,
	))
}
