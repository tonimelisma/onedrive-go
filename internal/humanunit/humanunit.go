// Package humanunit renders durations and sizes in the human units the
// output guidelines ask for ("6 s", "2 min 10 s", "300.0 MB"), the one place
// the product formats them for a person to read. It imports nothing from
// this repository. Machine output (--json fields) keeps its numbers and does
// not come here.
package humanunit

import (
	"fmt"
	"time"
)

// Precision says what a duration is, which decides how finely it is told.
type Precision int

const (
	// Wait is a wait, a delay or a limit the program sets: whole seconds,
	// never below "1 s", since a wait of "0 s" reads as no wait at all.
	Wait Precision = iota + 1
	// Measured is a time the program measured: tenths of a second below a
	// minute ("0.4 s", "12.3 s"), and from a minute up the same units as a
	// wait.
	Measured
)

// measuredStep is the resolution of a measured time below a minute.
const measuredStep = 100 * time.Millisecond

// Duration renders d at precision p. The value is rounded first and split
// after, so 59.96 s measured is "1 min", never "60.0 s", and 3599.5 s is
// "1 h". Below a minute it is seconds; from a minute up whole minutes and
// seconds, a zero part dropped ("2 min", "2 min 10 s"); from an hour up
// hours and whole minutes ("1 h", "1 h 15 min"). A negative d is zero.
func Duration(d time.Duration, p Precision) string {
	if d < 0 {
		d = 0
	}

	if p == Measured {
		if tenths := d.Round(measuredStep); tenths < time.Minute {
			return fmt.Sprintf("%.1f s", tenths.Seconds())
		}
	}

	seconds := max(d.Round(time.Second), time.Second)

	switch {
	case seconds < time.Minute:
		return fmt.Sprintf("%d s", int64(seconds/time.Second))
	case seconds < time.Hour:
		minutes, rest := int64(seconds/time.Minute), int64(seconds%time.Minute/time.Second)
		if rest == 0 {
			return fmt.Sprintf("%d min", minutes)
		}

		return fmt.Sprintf("%d min %d s", minutes, rest)
	default:
		whole := d.Round(time.Minute)
		hours, minutes := int64(whole/time.Hour), int64(whole%time.Hour/time.Minute)
		if minutes == 0 {
			return fmt.Sprintf("%d h", hours)
		}

		return fmt.Sprintf("%d h %d min", hours, minutes)
	}
}

// Binary size units: a kilobyte here is 1024 bytes.
const (
	kilobyte = 1 << 10
	megabyte = 1 << 20
	gigabyte = 1 << 30
	terabyte = 1 << 40
)

// Size renders a byte count with one decimal in the largest unit it reaches
// ("1.5 GB", "300.0 KB"), and below a kilobyte as whole bytes ("12 B").
func Size(bytes int64) string {
	switch {
	case bytes >= terabyte:
		return fmt.Sprintf("%.1f TB", float64(bytes)/terabyte)
	case bytes >= gigabyte:
		return fmt.Sprintf("%.1f GB", float64(bytes)/gigabyte)
	case bytes >= megabyte:
		return fmt.Sprintf("%.1f MB", float64(bytes)/megabyte)
	case bytes >= kilobyte:
		return fmt.Sprintf("%.1f KB", float64(bytes)/kilobyte)
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}
