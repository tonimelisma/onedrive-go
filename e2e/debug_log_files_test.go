package e2e

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/testutil"
)

// The debug log helpers take the roles they redact with, so the recorders
// that write a debug log line run without an account; the live suite calls
// them with its own roles (debugLogFileName, appendDebugLogChunk).

// sanitizeTestName replaces characters invalid in filenames.
func sanitizeTestName(name string) string {
	return strings.NewReplacer("/", "_", " ", "_", ":", "_").Replace(name)
}

// debugLogFileNameWith is a test's debug log file name: the test's name, any
// drive roles knows replaced by its role first, because the file is a CI
// artifact of a public repository and its name goes with it.
func debugLogFileNameWith(roles testutil.DriveRoles, testName string) string {
	return sanitizeTestName(roles.Redact(testName)) + ".log"
}

// appendDebugLogChunkWith appends content to a per-test debug log through
// roles.Redact, the command line, shared --json and drive list --json
// included: every account, drive, item and fixture of the suite named by role
// and any other identifier masked, because the logs are uploaded as CI
// artifacts of a public repository, where log masking does not reach.
func appendDebugLogChunkWith(roles testutil.DriveRoles, path string, content string) (err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // the per-test debug log under the suite's own log dir
	if err != nil {
		return fmt.Errorf("opening debug log %s: %w", path, err)
	}
	defer func() {
		closeErr := f.Close()
		if err == nil {
			err = closeErr
		}
	}()

	if _, err = f.WriteString(roles.Redact(content)); err != nil {
		return fmt.Errorf("writing debug log %s: %w", path, err)
	}

	return nil
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // callers pass paths inside their own sync dir or temp dir
	require.NoError(t, err)
	return data
}
