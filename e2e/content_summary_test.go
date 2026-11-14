package e2e

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// This file has no build tags, so its test runs in the untagged quick lane
// without an account; the live waits that print the summary build on it.

// contentSummaryMaxBytes bounds how much of a file a failure message shows.
const contentSummaryMaxBytes = 200

// summarizeContent describes data for a failure message: its length and at
// most its first contentSummaryMaxBytes bytes, quoted so that binary content
// cannot break the output.
func summarizeContent(data []byte) string {
	shown := data[:min(len(data), contentSummaryMaxBytes)]

	return fmt.Sprintf("%d bytes, first %d: %q", len(data), len(shown), shown)
}

// A failed content wait shows what it last downloaded: the length, a bounded
// prefix, and binary bytes escaped rather than written raw.
func TestSummarizeContentBoundsAndQuotes(t *testing.T) {
	t.Parallel()

	assert.Equal(t, `0 bytes, first 0: ""`, summarizeContent(nil))
	assert.Equal(t, `5 bytes, first 5: "a\x00\nb\xff"`, summarizeContent([]byte("a\x00\nb\xff")))

	shown := strings.Repeat("x", contentSummaryMaxBytes)
	long := shown + "the part past the bound"
	assert.Equal(t,
		fmt.Sprintf("%d bytes, first %d: %q", len(long), contentSummaryMaxBytes, shown),
		summarizeContent([]byte(long)))
}
