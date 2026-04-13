//go:build e2e

package e2e

import "time"

// sleepForLiveTestPropagation centralizes the rare real-clock waits that live
// E2E tests still need when observing external process startup and OneDrive
// propagation windows. In-process unit tests must use explicit synchronization
// instead.
func sleepForLiveTestPropagation(delay time.Duration) {
	//nolint:forbidigo // live E2E waits on external processes and provider visibility windows
	time.Sleep(delay)
}
