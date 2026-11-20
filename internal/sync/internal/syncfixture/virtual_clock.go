package syncfixture

import (
	"testing"
	gosynctest "testing/synctest"
	"time"
)

func InVirtualTime(t *testing.T, fn func(t *testing.T)) {
	t.Helper()

	gosynctest.Test(t, fn)
}

// VirtualWait lets d of virtual time pass. Only meaningful inside
// inVirtualTime, where it returns as soon as everything else is idle.
func VirtualWait(d time.Duration) {
	<-time.After(d)
}
