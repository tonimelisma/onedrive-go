package sync

import (
	"testing"
	gosynctest "testing/synctest"
	"time"
)

func inVirtualTime(t *testing.T, fn func(t *testing.T)) {
	t.Helper()

	gosynctest.Test(t, fn)
}

// virtualWait lets d of virtual time pass. Only meaningful inside
// inVirtualTime, where it returns as soon as everything else is idle.
func virtualWait(d time.Duration) {
	<-time.After(d)
}
