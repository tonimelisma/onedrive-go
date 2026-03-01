package sync

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

func testCanonicalID(t *testing.T, s string) driveid.CanonicalID {
	t.Helper()

	cid, err := driveid.NewCanonicalID(s)
	require.NoError(t, err)

	return cid
}

func TestDriveRunner_Run_Success(t *testing.T) {
	cid := testCanonicalID(t, "personal:test@example.com")
	dr := &DriveRunner{
		canonID:     cid,
		displayName: "Test Drive",
	}

	report := &SyncReport{
		Mode:      SyncBidirectional,
		Downloads: 3,
		Uploads:   2,
	}

	result := dr.run(context.Background(), func(_ context.Context) (*SyncReport, error) {
		return report, nil
	})

	assert.Equal(t, cid, result.CanonicalID)
	assert.Equal(t, "Test Drive", result.DisplayName)
	assert.NoError(t, result.Err)
	require.NotNil(t, result.Report)
	assert.Equal(t, 3, result.Report.Downloads)
	assert.Equal(t, 2, result.Report.Uploads)
}

func TestDriveRunner_Run_Error(t *testing.T) {
	cid := testCanonicalID(t, "personal:test@example.com")
	dr := &DriveRunner{
		canonID:     cid,
		displayName: "Failing Drive",
	}

	errSync := errors.New("delta token expired")

	result := dr.run(context.Background(), func(_ context.Context) (*SyncReport, error) {
		return nil, errSync
	})

	assert.Equal(t, cid, result.CanonicalID)
	assert.Equal(t, "Failing Drive", result.DisplayName)
	assert.ErrorIs(t, result.Err, errSync)
	assert.Nil(t, result.Report)
}

func TestDriveRunner_Run_Panic(t *testing.T) {
	cid := testCanonicalID(t, "personal:panic@example.com")
	dr := &DriveRunner{
		canonID:     cid,
		displayName: "Panic Drive",
	}

	result := dr.run(context.Background(), func(_ context.Context) (*SyncReport, error) {
		panic("nil pointer dereference in observer")
	})

	assert.Equal(t, cid, result.CanonicalID)
	assert.Equal(t, "Panic Drive", result.DisplayName)
	assert.Nil(t, result.Report)
	require.Error(t, result.Err)
	assert.Contains(t, result.Err.Error(), "panic")
	assert.Contains(t, result.Err.Error(), "nil pointer dereference in observer")
}

func TestDriveRunner_Run_PanicWithError(t *testing.T) {
	cid := testCanonicalID(t, "personal:panic-err@example.com")
	dr := &DriveRunner{
		canonID:     cid,
		displayName: "Panic Error Drive",
	}

	errPanic := fmt.Errorf("some internal error")

	result := dr.run(context.Background(), func(_ context.Context) (*SyncReport, error) {
		panic(errPanic)
	})

	assert.Nil(t, result.Report)
	require.Error(t, result.Err)
	assert.Contains(t, result.Err.Error(), "panic")
}

func TestDriveRunner_Run_ContextCanceled(t *testing.T) {
	cid := testCanonicalID(t, "personal:cancel@example.com")
	dr := &DriveRunner{
		canonID:     cid,
		displayName: "Cancel Drive",
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result := dr.run(ctx, func(c context.Context) (*SyncReport, error) {
		return nil, c.Err()
	})

	assert.ErrorIs(t, result.Err, context.Canceled)
	assert.Nil(t, result.Report)
}

func TestBackoffDuration(t *testing.T) {
	tests := []struct {
		name     string
		failures int
		want     time.Duration
	}{
		{name: "zero failures", failures: 0, want: 0},
		{name: "one failure", failures: 1, want: 0},
		{name: "two failures", failures: 2, want: 0},
		{name: "three failures", failures: 3, want: 1 * time.Minute},
		{name: "four failures", failures: 4, want: 5 * time.Minute},
		{name: "five failures", failures: 5, want: 15 * time.Minute},
		{name: "six failures", failures: 6, want: 1 * time.Hour},
		{name: "seven failures capped", failures: 7, want: 1 * time.Hour},
		{name: "hundred failures capped", failures: 100, want: 1 * time.Hour},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := backoffDuration(tt.failures)
			assert.Equal(t, tt.want, got)
		})
	}
}
