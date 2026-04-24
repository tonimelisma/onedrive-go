package multisync

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

// Validates: R-2.4
func TestMountRunner_Run_Success(t *testing.T) {
	cid := testCanonicalID(t, "personal:test@example.com")
	dr := &MountRunner{
		identity:    testStandaloneMountIdentity(cid),
		displayName: "Test Drive",
	}

	report := &syncengine.Report{
		Mode:      syncengine.SyncBidirectional,
		Downloads: 3,
		Uploads:   2,
	}

	result := dr.run(t.Context(), func(_ context.Context) (*syncengine.Report, error) {
		return report, nil
	})

	assert.Equal(t, testStandaloneMountIdentity(cid), result.Identity)
	assert.Equal(t, "Test Drive", result.DisplayName)
	require.NoError(t, result.Err)
	require.NotNil(t, result.Report)
	assert.Equal(t, 3, result.Report.Downloads)
	assert.Equal(t, 2, result.Report.Uploads)
}

// Validates: R-2.4
func TestMountRunner_Run_Error(t *testing.T) {
	cid := testCanonicalID(t, "personal:test@example.com")
	dr := &MountRunner{
		identity:    testStandaloneMountIdentity(cid),
		displayName: "Failing Drive",
	}

	errSync := errors.New("delta token expired")

	result := dr.run(t.Context(), func(_ context.Context) (*syncengine.Report, error) {
		return nil, errSync
	})

	assert.Equal(t, testStandaloneMountIdentity(cid), result.Identity)
	assert.Equal(t, "Failing Drive", result.DisplayName)
	require.ErrorIs(t, result.Err, errSync)
	assert.Nil(t, result.Report)
}

// Validates: R-6.8
func TestMountRunner_Run_Panic(t *testing.T) {
	cid := testCanonicalID(t, "personal:panic@example.com")
	dr := &MountRunner{
		identity:    testStandaloneMountIdentity(cid),
		displayName: "Panic Drive",
	}

	result := dr.run(t.Context(), func(_ context.Context) (*syncengine.Report, error) {
		panic("nil pointer dereference in observer")
	})

	assert.Equal(t, testStandaloneMountIdentity(cid), result.Identity)
	assert.Equal(t, "Panic Drive", result.DisplayName)
	assert.Nil(t, result.Report)
	require.Error(t, result.Err)
	assert.Contains(t, result.Err.Error(), "panic")
	assert.Contains(t, result.Err.Error(), "nil pointer dereference in observer")
}

// Validates: R-6.8
func TestMountRunner_Run_PanicWithError(t *testing.T) {
	cid := testCanonicalID(t, "personal:panic-err@example.com")
	dr := &MountRunner{
		identity:    testStandaloneMountIdentity(cid),
		displayName: "Panic Error Drive",
	}

	errPanic := fmt.Errorf("some internal error")

	result := dr.run(t.Context(), func(_ context.Context) (*syncengine.Report, error) {
		panic(errPanic)
	})

	assert.Nil(t, result.Report)
	require.Error(t, result.Err)
	assert.Contains(t, result.Err.Error(), "panic")
}

// Validates: R-2.4
func TestMountRunner_Run_ContextCanceled(t *testing.T) {
	cid := testCanonicalID(t, "personal:cancel@example.com")
	dr := &MountRunner{
		identity:    testStandaloneMountIdentity(cid),
		displayName: "Cancel Drive",
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	result := dr.run(ctx, func(c context.Context) (*syncengine.Report, error) {
		return nil, c.Err()
	})

	require.ErrorIs(t, result.Err, context.Canceled)
	assert.Nil(t, result.Report)
}
