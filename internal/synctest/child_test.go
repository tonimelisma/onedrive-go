package synctest

import (
	"testing"

	"github.com/stretchr/testify/assert"
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
func TestRunInChild_APassingChildPasses(t *testing.T) {
	t.Parallel()

	if InChild(t) {
		assert.True(t, InChild(t))
		return
	}

	assert.False(t, InChild(t), "the parent is not the child")
	RequireChildPassed(t, RunInChild(t))
}
