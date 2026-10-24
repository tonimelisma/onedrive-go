package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Validates: R-6.6.30
func TestFormatWatchConditionBreakdown_UsesUserWords(t *testing.T) {
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
		"1 file waiting for sign-in, 2 files in read-only shared folders",
		formatWatchConditionBreakdown(summary),
	)
}

// Validates: R-6.6.30
func TestWatchConditionSummaryFingerprint_IsTheSetOfConditionKinds(t *testing.T) {
	t.Parallel()

	summary := watchConditionSummary{
		Counts: []watchConditionCount{
			{Key: ConditionInvalidFilename, Count: 4},
		},
		ConditionTotal: 4,
	}

	assert.Equal(t, "invalid_filename", watchConditionSummaryFingerprint(summary),
		"counts are left out so a count change alone does not re-report")
}
