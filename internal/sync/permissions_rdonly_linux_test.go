//go:build linux

package sync

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// firstReadOnlyMount returns a readable mount point the kernel lists as
// read-only, or "" when the host has none.
func firstReadOnlyMount(t *testing.T) string {
	t.Helper()

	data, err := os.ReadFile("/proc/self/mounts")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		for _, opt := range strings.Split(fields[3], ",") {
			if opt != "ro" {
				continue
			}
			if _, statErr := os.Stat(fields[1]); statErr == nil { //nolint:gosec // a kernel-listed mount point, only stat'ed
				return fields[1]
			}
		}
	}

	return ""
}

// Validates: R-2.10.61
func TestMountIsReadOnly_ReadsMountFlags(t *testing.T) {
	t.Parallel()

	readOnly, known := mountIsReadOnly(t.TempDir())
	require.True(t, known)
	assert.False(t, readOnly)
	assert.False(t, isOnReadOnlyFilesystem(t.TempDir()))

	mount := firstReadOnlyMount(t)
	if mount == "" {
		t.Skip("no read-only mount on this host")
	}
	readOnly, known = mountIsReadOnly(mount)
	require.True(t, known)
	assert.True(t, readOnly, "%s is mounted ro", mount)
	assert.True(t, isOnReadOnlyFilesystem(mount))
}
