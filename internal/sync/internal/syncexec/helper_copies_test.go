package syncexec

// Private copies of test helpers whose home (syncobserve) these tests
// cannot import, because it imports this package or is another package's
// tests.

import (
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncobserve"
)

// localBatches installs a buffered local observation channel on obs, the one
// output the watch runtime installs, and returns it.
func localBatches(obs *syncobserve.LocalObserver, size int) chan syncobserve.LocalObservation {
	batches := make(chan syncobserve.LocalObservation, size)
	obs.SetLocalObservationBatchChannel(batches)

	return batches
}

// drainLocalPatches returns every local patch batches holds now, without
// waiting.
func drainLocalPatches(batches <-chan syncobserve.LocalObservation) []syncobserve.LocalObservation {
	var drained []syncobserve.LocalObservation
	for {
		select {
		case batch := <-batches:
			if batch.Kind == syncobserve.LocalPatch {
				drained = append(drained, batch)
			}
		default:
			return drained
		}
	}
}
