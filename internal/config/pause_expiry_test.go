package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-2.8.12
//
// PauseExpiry is what the watch arms its wake-up from: only a parseable
// paused_until on a paused drive is a time to wake up at. The other states
// read exactly as IsPaused reads them, so the two can never disagree about
// whether a pause ends on its own.
func TestDrivePauseExpiry_OnlyATimedPauseHasAnEnd(t *testing.T) {
	t.Parallel()

	paused := true
	notPaused := false
	stamp := "2026-11-02T22:30:16-08:00"
	want, err := time.Parse(time.RFC3339, stamp)
	require.NoError(t, err)
	garbage := "not a time"

	cases := []struct {
		name  string
		drive Drive
		ok    bool
	}{
		{name: "not paused", drive: Drive{PausedUntil: &stamp}},
		{name: "paused false", drive: Drive{Paused: &notPaused, PausedUntil: &stamp}},
		{name: "indefinite", drive: Drive{Paused: &paused}},
		{name: "unparseable", drive: Drive{Paused: &paused, PausedUntil: &garbage}},
		{name: "timed", drive: Drive{Paused: &paused, PausedUntil: &stamp}, ok: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			until, ok := tc.drive.PauseExpiry()
			assert.Equal(t, tc.ok, ok)
			if tc.ok {
				assert.True(t, until.Equal(want))
			} else {
				assert.True(t, until.IsZero())
			}
			// The end PauseExpiry reports is the moment IsPaused flips.
			if tc.ok {
				assert.True(t, tc.drive.IsPaused(want.Add(-time.Second)))
				assert.False(t, tc.drive.IsPaused(want))
			}
		})
	}
}
