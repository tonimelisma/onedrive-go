package syncstore

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// Validates: R-2.10.7, R-2.10.14
func TestStoredTimeNano_SaturatesAtBothEndsOfTheRange(t *testing.T) {
	t.Parallel()

	latest := time.Unix(0, math.MaxInt64)
	earliest := time.Unix(0, math.MinInt64)
	inRange := time.Date(2025, 6, 15, 12, 0, 0, 123456789, time.UTC)

	tests := []struct {
		name string
		at   time.Time
		want int64
	}{
		{name: "the zero time is no time", at: time.Time{}, want: 0},
		{name: "a time in range is its Unix nanoseconds", at: inRange, want: inRange.UnixNano()},
		{name: "the epoch stores one nanosecond after it, zero being no time", at: time.Unix(0, 0), want: 1},
		{name: "the last storable time", at: latest, want: math.MaxInt64},
		{name: "one nanosecond past it", at: latest.Add(time.Nanosecond), want: math.MaxInt64},
		{name: "a saturated Retry-After from now", at: inRange.Add(time.Duration(math.MaxInt64)), want: math.MaxInt64},
		{name: "the first storable time", at: earliest, want: math.MinInt64},
		{name: "one nanosecond before it", at: earliest.Add(-time.Nanosecond), want: math.MinInt64},
		{name: "a time long before the range", at: time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC), want: math.MinInt64},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			stored := syncdomain.StoredTimeNano(tc.at)
			assert.Equal(t, tc.want, stored)
			assert.Equal(t, !tc.at.IsZero(), syncdomain.StoredTimeIsSet(stored), "only the zero time stores no time")
			if !tc.at.IsZero() {
				// Order against a time in range survives the round trip.
				readBack := syncdomain.RemoteModTime(stored)
				assert.Equal(t, tc.at.Compare(inRange), readBack.Compare(inRange))
			}
		})
	}
}

// Validates: R-6.7.34
func TestRemoteModTime_DecodesAStoredRemoteModificationTime(t *testing.T) {
	t.Parallel()

	assert.True(t, syncdomain.RemoteModTime(0).IsZero(), "no time is known: the zero time, which nothing sets")
	assert.True(t, syncdomain.RemoteModTime(math.MaxInt64).Equal(time.Unix(0, math.MaxInt64)))
	assert.True(t, syncdomain.RemoteModTime(math.MinInt64).Equal(time.Unix(0, math.MinInt64)))
	at := time.Date(2025, 6, 15, 12, 0, 0, 123456789, time.UTC)
	assert.True(t, syncdomain.RemoteModTime(syncdomain.StoredTimeNano(at)).Equal(at))
}
