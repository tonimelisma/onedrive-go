package namekey

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Validates: R-2.13.2
func TestOtherNormalForms(t *testing.T) {
	t.Parallel()

	nfc, nfd := "café", "café"
	assert.Equal(t, []string{nfd}, OtherNormalForms(nfc))
	assert.Equal(t, []string{nfc}, OtherNormalForms(nfd))
	assert.Empty(t, OtherNormalForms("plain.txt"))
	// A mixed spelling, neither NFC nor NFD, has both forms as others.
	mixed := "café café"
	assert.Equal(t, []string{"café café", "café café"}, OtherNormalForms(mixed))
}
