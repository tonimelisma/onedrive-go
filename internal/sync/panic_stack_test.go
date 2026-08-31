package sync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-6.7.1
//
// A recovered panic reports what failed but not where, and the goroutine that
// could answer that is unwound by the time anyone reads the report. Every
// recovery site that logs must therefore capture the stack; a daemon that
// survives a panic without recording where it happened has traded a crash for
// an unactionable log line.
func TestPanicRecoverySitesCaptureTheStack(t *testing.T) {
	t.Parallel()

	roots := []string{
		filepath.Join("..", "sync"),
		filepath.Join("..", "multisync"),
	}

	checked := 0

	for _, root := range roots {
		entries, err := os.ReadDir(root)
		require.NoError(t, err)

		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || filepath.Ext(name) != ".go" || isPanicStackRuleOwnFile(name) {
				continue
			}

			data, readErr := os.ReadFile(filepath.Join(root, name)) //nolint:gosec // repo-relative test input
			require.NoError(t, readErr)

			for _, window := range panicRecoveryWindows(string(data)) {
				if !strings.Contains(window, ".Error(") {
					continue // a recovery that does not log has no stack to record
				}

				checked++

				assert.Contains(t, window, "debug.Stack()",
					"%s logs a recovered panic without its stack", filepath.Join(root, name))
			}
		}
	}

	assert.NotZero(t, checked, "the scan must find the recovery sites it is meant to police")
}

// panicRecoveryWindows returns the source following each recover() call, far
// enough to cover the log statement that reports it.
func panicRecoveryWindows(source string) []string {
	const windowBytes = 900

	var windows []string

	for offset := 0; ; {
		idx := strings.Index(source[offset:], "recover()")
		if idx < 0 {
			return windows
		}

		start := offset + idx
		end := min(start+windowBytes, len(source))
		windows = append(windows, source[start:end])
		offset = start + len("recover()")
	}
}

func isPanicStackRuleOwnFile(name string) bool {
	return name == "panic_stack_test.go"
}
