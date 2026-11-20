//go:build race

package syncfixture

// RaceDetectorEnabled reports whether this test binary was built with -race.
// The race detector slows the model-based tests about tenfold, so they run
// fewer seeds there (see modelSeeds in package sync).
const RaceDetectorEnabled = true
