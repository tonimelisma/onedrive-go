//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// requireDrive2Shared skips shared-account tests when the second drive is not
// configured in the live E2E environment.
func requireDrive2Shared(t *testing.T) {
	t.Helper()

	if drive2 == "" {
		t.Skip("ONEDRIVE_TEST_DRIVE_2 not set — skipping shared-account test")
	}
}

func recipientEmailFromDriveID(t *testing.T, driveID string) string {
	t.Helper()

	parts := strings.SplitN(driveID, ":", 2)
	require.Len(t, parts, 2)

	return parts[1]
}

// writeSyncConfigForDrive2 creates a per-test config pointing to drive2.
// The recipient account is used by shared-item caller-proof tests.
func writeSyncConfigForDrive2(t *testing.T, syncDir string) (string, map[string]string) {
	t.Helper()

	perTestData := t.TempDir()
	perTestHome := t.TempDir()

	perTestDataDir := filepath.Join(perTestData, "onedrive-go")
	require.NoError(t, os.MkdirAll(perTestDataDir, 0o700))
	copyTokenFileForDrive(t, testDataDir, perTestDataDir, drive2)

	content := fmt.Sprintf(`["%s"]
sync_dir = %q
`, drive2, syncDir)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0o600))

	env := map[string]string{
		"XDG_DATA_HOME": perTestData,
		"HOME":          perTestHome,
	}

	return cfgPath, env
}
