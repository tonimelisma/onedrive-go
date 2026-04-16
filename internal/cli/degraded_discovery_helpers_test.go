package cli

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-3.3.1, R-6.8.16
func TestDegradedDiscoveryHelpers(t *testing.T) {
	t.Parallel()

	assert.Empty(t, degradedReasonText("unknown"))
	assert.Empty(t, degradedAction("unknown"))
	assert.NotContains(t, degradedReasonText(driveCatalogUnavailableReason), "temporarily unavailable")
	assert.NotContains(t, degradedAction(driveCatalogUnavailableReason), "few seconds")
	assert.Contains(t, degradedAction(driveCatalogUnavailableReason), "No action is needed")
	assert.Contains(t, degradedAction(driveCatalogUnavailableReason), "again later")
	assert.Contains(t, degradedAction(sharedDiscoveryUnavailableReason), "known")

	merged := mergeDegradedNotices(
		[]accountDegradedNotice{
			{Email: "b@example.com", Reason: driveCatalogUnavailableReason},
			{Email: "", Reason: driveCatalogUnavailableReason},
		},
		[]accountDegradedNotice{
			{
				Email:       "a@example.com",
				DisplayName: "Alice",
				DriveType:   "personal",
				Reason:      sharedDiscoveryUnavailableReason,
				Action:      degradedAction(sharedDiscoveryUnavailableReason),
			},
			{
				Email:       "b@example.com",
				DisplayName: "Bob",
				DriveType:   "business",
				Reason:      driveCatalogUnavailableReason,
				Action:      degradedAction(driveCatalogUnavailableReason),
			},
		},
	)

	require.Len(t, merged, 2)
	assert.Equal(t, "a@example.com", merged[0].Email)
	assert.Equal(t, "b@example.com", merged[1].Email)
	assert.Equal(t, "Bob", merged[1].DisplayName)
	assert.Equal(t, "business", merged[1].DriveType)
	assert.Equal(t, degradedAction(driveCatalogUnavailableReason), merged[1].Action)

	var out bytes.Buffer
	err := printAccountDegradedText(&out, "Degraded accounts:", merged)
	require.NoError(t, err)
	assert.Contains(t, out.String(), "Degraded accounts:")
	assert.Contains(t, out.String(), "Alice (a@example.com)")
	assert.Contains(t, out.String(), degradedReasonText(sharedDiscoveryUnavailableReason))
	assert.Contains(t, out.String(), degradedAction(driveCatalogUnavailableReason))

	out.Reset()
	require.NoError(t, printAccountDegradedText(&out, "Ignored", nil))
	assert.Empty(t, out.String())
}
