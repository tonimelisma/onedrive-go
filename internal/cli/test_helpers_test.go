package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/multisync"
)

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
