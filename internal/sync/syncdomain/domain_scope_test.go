package syncdomain

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Validates: R-2.10.1, R-2.10.3, R-2.10.65
func TestScopeKeyForResult(t *testing.T) {
	t.Parallel()

	key := func(status int, drive driveid.ID) ScopeKey {
		return ScopeKeyForResult(classifyServerAnswer(status, nil), drive)
	}
	assert.Equal(t, SKThrottleDrive(driveid.New("0000000000000001")), key(429, driveid.New("0000000000000001")))
	assert.True(t, key(429, driveid.ID{}).IsZero(), "429 without a drive id should be zero")
	assert.Equal(t, SKService(), key(503, driveid.ID{}))
	assert.Equal(t, SKService(), key(500, driveid.ID{}))
	assert.Equal(t, SKService(), key(502, driveid.ID{}))
	assert.Equal(t, SKQuotaOwn(), key(507, driveid.ID{}))
	assert.True(t, key(501, driveid.ID{}).IsZero(), "a refusal is not service evidence")
	assert.True(t, key(505, driveid.ID{}).IsZero(), "a refusal is not service evidence")
	assert.True(t, key(404, driveid.ID{}).IsZero(), "non-scope status should be zero")
	assert.True(t, key(200, driveid.ID{}).IsZero(), "success status should be zero")
}
