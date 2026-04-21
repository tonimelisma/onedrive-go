package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Validates: R-2.10.47
func TestFormatWatchConditionBreakdown_UsesRawCountsOnly(t *testing.T) {
	t.Parallel()

	summary := watchConditionSummary{
		Counts: []watchConditionCount{
			{Key: ConditionAuthenticationRequired, Count: 1},
			{Key: ConditionRemoteWriteDenied, Count: 2},
		},
		ConditionTotal: 3,
		Retrying:       99,
	}

	assert.Equal(t,
		"1 authentication_required, 2 remote_write_denied",
		formatWatchConditionBreakdown(summary),
	)
}

// Validates: R-2.10.47
func TestWatchConditionSummaryFingerprint_IncludesTotalAndBreakdown(t *testing.T) {
	t.Parallel()

	summary := watchConditionSummary{
		Counts: []watchConditionCount{
			{Key: ConditionInvalidFilename, Count: 4},
		},
		ConditionTotal: 4,
	}

	assert.Equal(t, "4|invalid_filename=4", watchConditionSummaryFingerprint(summary))
}
