//go:build !race

package sync

// raceDetectorEnabled reports whether this test binary was built with -race.
const raceDetectorEnabled = false
