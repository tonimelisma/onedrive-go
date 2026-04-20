package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

func TestScopeKey_StringParseRoundTrip(t *testing.T) {
	t.Parallel()

	keys := []ScopeKey{
		SKThrottleDrive(driveid.New("0000000000000001")),
		SKService(),
		SKQuotaOwn(),
		SKPermLocalWrite("/docs"),
		SKPermRemoteWrite(""),
		SKDiskLocal(),
	}

	for _, key := range keys {
		t.Run(key.String(), func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, key, ParseScopeKey(key.String()))
		})
	}
}

func TestParseScopeKey_UnknownReturnsZero(t *testing.T) {
	t.Parallel()

	assert.True(t, ParseScopeKey("not-a-scope").IsZero())
}

// Validates: R-6.8.4
func TestScopeKey_ThrottleTargetAccessors(t *testing.T) {
	t.Parallel()

	driveID := driveid.New("0000000000000001")
	driveKey := SKThrottleDrive(driveID)

	assert.True(t, driveKey.IsThrottleTarget())
	assert.True(t, driveKey.IsThrottleDrive())
	assert.Equal(t, throttleDriveParam(driveID), driveKey.ThrottleTargetKey())

	assert.False(t, SKQuotaOwn().IsThrottleTarget())
	assert.False(t, SKQuotaOwn().IsThrottleDrive())
}

// Validates: R-6.8.4
func TestScopeKey_ThrottleTargetKeyPanicsForNonTarget(t *testing.T) {
	t.Parallel()

	require.Panics(t, func() {
		_ = SKQuotaOwn().ThrottleTargetKey()
	})
}

func TestScopeKey_IsPermDirAndDirPath(t *testing.T) {
	t.Parallel()

	key := SKPermLocalWrite("/docs")
	assert.True(t, key.IsPermDir())
	assert.Equal(t, "/docs", key.DirPath())
}

func TestScopeKey_IsPermRemoteAndRemotePath(t *testing.T) {
	t.Parallel()

	key := SKPermRemoteWrite("/readonly")
	assert.True(t, key.IsPermRemote())
	assert.Equal(t, "/readonly", key.RemotePath())
}

func TestScopeKey_DirPathPanicsForNonPermDir(t *testing.T) {
	t.Parallel()

	require.Panics(t, func() {
		_ = SKQuotaOwn().DirPath()
	})
}

func TestScopeKey_RemotePathPanicsForNonPermRemote(t *testing.T) {
	t.Parallel()

	require.Panics(t, func() {
		_ = SKQuotaOwn().RemotePath()
	})
}
