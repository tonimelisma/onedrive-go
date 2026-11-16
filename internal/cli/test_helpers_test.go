package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/multisync"
)

// eventWait is how long a test waits for an event that must come: a signal's
// cancellation, a helper process's line or exit, a goroutine's unwinding, a
// file a started command writes. A wait returns the moment its event arrives,
// so a passing test never spends it; it only bounds how long a broken one
// hangs. It is long because a loaded two-core host under the race detector
// takes seconds for what takes milliseconds alone, and shorter bounds failed
// tests there that were correct. A deadline that is the subject of a test
// keeps its own name beside that test.
const eventWait = time.Minute

func writeTestResponse(t *testing.T, w io.Writer, body string) {
	t.Helper()

	_, err := io.WriteString(w, body)
	require.NoError(t, err)
}

func writeTestResponsef(t *testing.T, w io.Writer, format string, args ...any) {
	t.Helper()

	_, err := fmt.Fprintf(w, format, args...)
	require.NoError(t, err)
}

func removeTestPath(t *testing.T, path string) {
	t.Helper()

	err := os.Remove(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		require.NoError(t, err)
	}
}

func testStandaloneMountIdentity(cid driveid.CanonicalID) multisync.MountIdentity {
	return multisync.MountIdentity{
		MountID:        cid.String(),
		ProjectionKind: multisync.MountProjectionStandalone,
		CanonicalID:    cid,
	}
}
