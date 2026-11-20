package sync

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/tonimelisma/onedrive-go/internal/graph"
)

func TestExtractRetryAfter_GraphError(t *testing.T) {
	t.Parallel()

	ge := &graph.GraphError{StatusCode: 429, RetryAfter: 30 * time.Second}
	assert.Equal(t, 30*time.Second, extractRetryAfter(ge))
}

func TestExtractRetryAfter_Nil(t *testing.T) {
	t.Parallel()

	assert.Equal(t, time.Duration(0), extractRetryAfter(nil))
}

// TestExtractRetryAfter_Wrapped verifies that ExtractRetryAfter works with wrapped errors.
func TestExtractRetryAfter_Wrapped(t *testing.T) {
	t.Parallel()

	ge := &graph.GraphError{StatusCode: 503, RetryAfter: 120 * time.Second}
	wrapped := fmt.Errorf("request failed: %w", ge)
	assert.Equal(t, 120*time.Second, extractRetryAfter(wrapped))
}
