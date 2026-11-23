package synctest

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

const (
	// childKillProbeBound is the bound the kill test gives its child, short
	// so the test ends soon after the child starts.
	childKillProbeBound = 2 * time.Second
	// childNeverEndsWait is longer than any bound: the child of the kill
	// test waits on a timer, not a bare receive, so the runtime does not end
	// it as a deadlock first.
	childNeverEndsWait = time.Hour
)

// Validates: R-6.3.10
//
// A goroutine panic with no boundary ends the process with Go's exit status
// 2. Run in a child, it ends only the child, and the parent sees how.
func TestRunInChild_AnEscapedGoroutinePanicEndsOnlyTheChild(t *testing.T) {
	t.Parallel()

	if InChild(t) {
		never := make(chan struct{})
		go func() {
			panic("escaped on purpose")
		}()
		<-never // the panic ends the child first
		return
	}

	result := RunInChild(t)
	assert.Equal(t, 2, result.ExitCode, result.Output)
	assert.Contains(t, result.Output, "panic: escaped on purpose")
}

// Validates: R-6.3.10
//
// A child that never ends is killed at its bound, and the output its parent
// reads says so, naming the bound, instead of a failure with no cause.
func TestRunInChild_AChildKilledAtItsBoundSaysSo(t *testing.T) {
	t.Parallel()

	if InChild(t) {
		<-time.After(childNeverEndsWait)
		return
	}

	result := runInChildWithin(t, childKillProbeBound)
	assert.NotEqual(t, 0, result.ExitCode, result.Output)
	assert.Contains(t, result.Output, "the child test binary was killed at its "+childKillProbeBound.String()+" bound")
}

// Validates: R-6.3.10
func TestRunInChild_APassingChildPasses(t *testing.T) {
	t.Parallel()

	if InChild(t) {
		assert.True(t, InChild(t))
		return
	}

	assert.False(t, InChild(t), "the parent is not the child")
	RequireChildPassed(t, RunInChild(t))
}
