//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type stderrStringer interface {
	String() string
}

func waitForSocketIOConnected(t *testing.T, stderr stderrStringer, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		output := stderr.String()
		switch {
		case strings.Contains(output, "socket.io connected"):
			return
		case strings.Contains(output, "socket.io endpoint fetch failed"):
			require.FailNow(t, "socket.io endpoint fetch failed", output)
		case strings.Contains(output, "socket.io connect failed"):
			require.FailNow(t, "socket.io connection failed", output)
		}

		time.Sleep(500 * time.Millisecond)
	}

	require.FailNow(t, "socket.io did not connect within the startup window", stderr.String())
}
