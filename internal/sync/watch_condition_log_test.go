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

// Every declared condition key is named in user words on purpose. The label
// table was a map with a fallback that would have worded a key it lacked by
// the key's own name, with nothing to notice; the fallback now answers only a
// key no constant declares.
// Validates: R-6.6.30
func TestConditionLabel_EveryDeclaredKeyHasALabel(t *testing.T) {
	t.Parallel()

	for name, value := range stringConstantsOfType(t, "condition_keys.go", "ConditionKey") {
		label, ok := conditionLabelText(ConditionKey(value))
		assert.True(t, ok, "%s has no label", name)
		assert.NotEmpty(t, label, "%s has an empty label", name)
		assert.NotContains(t, label, "_", "%s is labeled in words, not by its key", name)
	}

	label, ok := conditionLabelText(conditionKeyOutsideTheSet)
	assert.False(t, ok)
	assert.Empty(t, label)
	assert.Equal(t, "files with not a condition key", conditionLabel(conditionKeyOutsideTheSet, 2))
	assert.Equal(t, "file waiting for sign-in", conditionLabel(ConditionAuthenticationRequired, 1))
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
