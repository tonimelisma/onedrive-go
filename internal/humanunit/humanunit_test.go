package humanunit

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// Every boundary of both precisions: a wait is whole seconds and never
// below 1 s, a measured time tenths below a minute; the value is rounded
// before it is split, so nothing reads "60.0 s" or "60 min", a zero part is
// dropped and from an hour up the seconds are.
//
// Validates: R-6.6.38
func TestDuration_RendersBothPrecisionsAtEveryBoundary(t *testing.T) {
	t.Parallel()

	const tenth = 100 * time.Millisecond

	tests := []struct {
		name     string
		in       time.Duration
		wait     string
		measured string
	}{
		{name: "zero", in: 0, wait: "1 s", measured: "0.0 s"},
		{name: "negative", in: -3 * time.Second, wait: "1 s", measured: "0.0 s"},
		{name: "0.04 s", in: 40 * time.Millisecond, wait: "1 s", measured: "0.0 s"},
		{name: "0.96 s", in: 960 * time.Millisecond, wait: "1 s", measured: "1.0 s"},
		{name: "2.1 s", in: 21 * tenth, wait: "2 s", measured: "2.1 s"},
		{name: "59.94 s", in: 59940 * time.Millisecond, wait: "1 min", measured: "59.9 s"},
		{name: "59.96 s", in: 59960 * time.Millisecond, wait: "1 min", measured: "1 min"},
		{name: "60 s", in: time.Minute, wait: "1 min", measured: "1 min"},
		{name: "119.5 s", in: 1195 * tenth, wait: "2 min", measured: "2 min"},
		{name: "130 s", in: 130 * time.Second, wait: "2 min 10 s", measured: "2 min 10 s"},
		{name: "3599.5 s", in: 35995 * tenth, wait: "1 h", measured: "1 h"},
		{name: "3600 s", in: time.Hour, wait: "1 h", measured: "1 h"},
		{name: "4500 s", in: 4500 * time.Second, wait: "1 h 15 min", measured: "1 h 15 min"},
		{name: "4529 s", in: 4529 * time.Second, wait: "1 h 15 min", measured: "1 h 15 min"},
	}

	for _, tt := range tests {
		assert.Equal(t, tt.wait, Duration(tt.in, Wait), tt.name+" as a wait")
		assert.Equal(t, tt.measured, Duration(tt.in, Measured), tt.name+" measured")
	}
}

// Validates: R-6.6.38
func TestSize_RendersTheLargestUnitReached(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   int64
		want string
	}{
		{in: 0, want: "0 B"},
		{in: 512, want: "512 B"},
		{in: 1023, want: "1023 B"},
		{in: 1024, want: "1.0 KB"},
		{in: 1536, want: "1.5 KB"},
		{in: 5 << 20, want: "5.0 MB"},
		{in: 1536 << 20, want: "1.5 GB"},
		{in: 1 << 40, want: "1.0 TB"},
	}

	for _, tt := range tests {
		assert.Equal(t, tt.want, Size(tt.in))
	}
}
