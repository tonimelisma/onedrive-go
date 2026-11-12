//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/testutil"
)

// Validates: R-6.10.27
//
// The repository is public. A tracked file that spells a configured test
// account, or the account of an allowlist entry, publishes it. Only the live
// jobs know the addresses, so the check runs in the quick live tier, and a
// finding names the file, the line and the account's role, never the
// address.
func TestE2E_TrackedFilesNameNoTestAccount(t *testing.T) {
	root := findModuleRoot()
	//nolint:gosec // a fixed git listing of this repository
	out, err := exec.CommandContext(t.Context(), "git", "-C", root, "ls-files", "-z").Output()
	require.NoError(t, err)
	files := strings.Split(strings.TrimRight(string(out), "\x00"), "\x00")
	require.NotEmpty(t, files)

	driveIDs := append(liveConfig.CandidateDriveIDs(), strings.Split(os.Getenv(testutil.AllowedTestAccountsEnvVar), ",")...)
	findings, err := testutil.FindTestAccounts(root, files, driveIDs)
	require.NoError(t, err)
	for _, finding := range findings {
		assert.Fail(t, "a tracked file spells a test account; name it by role instead", finding.String())
	}
}
