package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// Validates: R-6.6.30
func TestFormatWatchConditionBreakdown_UsesUserWords(t *testing.T) {
	t.Parallel()

	summary := watchConditionSummary{
		Counts: []watchConditionCount{
			{Key: syncdomain.ConditionAuthenticationRequired, Count: 1},
			{Key: syncdomain.ConditionRemoteWriteDenied, Count: 2},
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

	for name, value := range syncfixture.StringConstantsOfType(t, "condition_keys.go", "ConditionKey") {
		label, ok := conditionLabelText(syncdomain.ConditionKey(value))
		assert.True(t, ok, "%s has no label", name)
		assert.NotEmpty(t, label, "%s has an empty label", name)
		assert.NotContains(t, label, "_", "%s is labeled in words, not by its key", name)
	}

	label, ok := conditionLabelText(syncfixture.ConditionKeyOutsideTheSet)
	assert.False(t, ok)
	assert.Empty(t, label)
	assert.Equal(t, "files with not a condition key", conditionLabel(syncfixture.ConditionKeyOutsideTheSet, 2))
	assert.Equal(t, "file waiting for sign-in", conditionLabel(syncdomain.ConditionAuthenticationRequired, 1))
}

// A condition of a mount point is a condition of one folder, the mount point,
// so the label counts folders, not files.
//
// Validates: R-6.6.30
func TestConditionLabel_AMountPointIsCountedAsAFolder(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "folder whose disk is not mounted", conditionLabel(syncdomain.ConditionLocalMountMissing, 1))
	assert.Equal(t, "folders on another filesystem inside the sync folder", conditionLabel(syncdomain.ConditionLocalMountBoundary, 2))
}

// Validates: R-6.6.30
func TestWatchConditionSummaryFingerprint_IsTheSetOfConditionKinds(t *testing.T) {
	t.Parallel()

	summary := watchConditionSummary{
		Counts: []watchConditionCount{
			{Key: syncdomain.ConditionInvalidFilename, Count: 4},
		},
		ConditionTotal: 4,
	}

	assert.Equal(t, "invalid_filename", watchConditionSummaryFingerprint(summary),
		"counts are left out so a count change alone does not re-report")
}
