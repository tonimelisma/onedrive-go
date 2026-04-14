package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Validates: R-3.1.3
func TestCanonicalIDHelpers(t *testing.T) {
	t.Parallel()

	personal := driveid.MustCanonicalID("personal:b@example.com")
	business := driveid.MustCanonicalID("business:a@example.com")

	assert.Positive(t, compareCanonicalID(personal, business))
	assert.Negative(t, compareCanonicalID(business, personal))
	assert.Zero(t, compareCanonicalID(personal, personal))

	ids := appendUniqueCanonicalID(nil, personal)
	ids = appendUniqueCanonicalID(ids, personal)
	ids = appendUniqueCanonicalID(ids, business)
	assert.Equal(t, []driveid.CanonicalID{personal, business}, ids)
	assert.Equal(t, personal, representativeTokenID([]driveid.CanonicalID{personal, business}))
}
