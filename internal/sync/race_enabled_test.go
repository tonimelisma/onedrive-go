//go:build race

package sync

// raceDetectorEnabled reports whether this test binary was built with -race.
// The race detector slows the model-based test about tenfold, so it runs
// fewer seeds there (see modelSeeds).
const raceDetectorEnabled = true
