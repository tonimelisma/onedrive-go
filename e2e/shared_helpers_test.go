//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func recipientEmailFromDriveID(t *testing.T, driveID string) string {
	t.Helper()

	email, err := recipientEmailFromCanonicalDriveID(driveID)
	require.NoError(t, err)

	return email
}

func recipientEmailFromCanonicalDriveID(driveID string) (string, error) {
	parts := strings.SplitN(driveID, ":", 2)
	if len(parts) != 2 || parts[1] == "" {
		return "", fmt.Errorf("parse recipient email from drive ID %q", driveID)
	}

	return parts[1], nil
}

func configuredDriveIDForRecipient(t *testing.T, recipientEmail string) string {
	t.Helper()

	driveID, ok := liveConfig.DriveIDForEmail(recipientEmail)
	require.Truef(t, ok,
		"shared fixture recipient %q does not match any configured drive (%v)",
		recipientEmail,
		liveConfig.CandidateDriveIDs(),
	)

	return driveID
}

// writeSyncConfigForDriveID creates a per-test config pointing to the given
// drive. Shared-item tests use this to execute commands as the actual
// recipient account instead of assuming a fixed drive slot.
func writeSyncConfigForDriveID(t *testing.T, driveID string, syncDir string) (string, map[string]string) {
	t.Helper()

	perTestData := t.TempDir()
	perTestHome := t.TempDir()

	perTestDataDir := filepath.Join(perTestData, "onedrive-go")
	require.NoError(t, os.MkdirAll(perTestDataDir, 0o700))
	copyTokenFileForDrive(t, testDataDir, perTestDataDir, driveID)

	content := fmt.Sprintf(`["%s"]
sync_dir = %q
`, driveID, syncDir)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0o600))

	env := map[string]string{
		"XDG_DATA_HOME": perTestData,
		"HOME":          perTestHome,
	}

	return cfgPath, env
}

func assertSyncLeavesLocalTreeStableForDrive(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	driveID string,
	root string,
	args ...string,
) string {
	t.Helper()

	before := snapshotLocalTree(t, root)
	_, stderr := runCLIWithConfigForDrive(t, cfgPath, env, driveID, args...)
	after := snapshotLocalTree(t, root)
	assert.Equal(t, before, after, "sync should not mutate the test-owned local tree")

	return stderr
}
