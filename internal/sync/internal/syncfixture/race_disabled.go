//go:build !race

package syncfixture

// RaceDetectorEnabled reports whether this test binary was built with -race.
const RaceDetectorEnabled = false
