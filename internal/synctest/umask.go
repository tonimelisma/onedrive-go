package synctest

import (
	"context"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// GroupWritableUmask is a umask that leaves group write, the default where
// every user has a group of their own (Debian, Ubuntu, Fedora). Under it a
// folder created with 0o777 and one created with 0o755 differ, which the
// common 0o022 hides.
const GroupWritableUmask os.FileMode = 0o002

// umaskChildEnv carries the umask to the child test binary RunUnderUmask
// starts, in octal.
const umaskChildEnv = "ONEDRIVE_GO_TEST_UMASK"

// umaskChildTimeout bounds the child test binary.
const umaskChildTimeout = 2 * time.Minute

// RunUnderUmask runs the calling test with the process umask set to umask
// and reports whether the caller is that run.
//
// The umask belongs to the whole process, so a test that sets it in a test
// binary running tests in parallel changes the mode of whatever the other
// tests create at that moment. The first call therefore starts the test
// binary again for this one test, with the umask in its environment, waits
// for it, fails the test when that run fails or ran no test, and returns
// false: the caller returns at once. In the child the call sets the umask
// and returns true, and the test does its work there. Only a top-level test
// may call it, before anything else it does.
func RunUnderUmask(t *testing.T, umask os.FileMode) bool {
	t.Helper()

	octal := strconv.FormatUint(uint64(umask), 8)
	if value, isChild := os.LookupEnv(umaskChildEnv); isChild {
		require.Equal(t, octal, value, "the child test binary was started for another umask")
		syscall.Umask(int(umask))

		return true
	}

	exe, err := os.Executable()
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), umaskChildTimeout)
	defer cancel()

	//nolint:gosec // runs this test binary again, for one test, with fixed arguments
	cmd := exec.CommandContext(ctx, exe, "-test.run=^"+regexp.QuoteMeta(t.Name())+"$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), umaskChildEnv+"="+octal)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s under umask %04o:\n%s", t.Name(), umask, output)
	require.Contains(t, string(output), "--- PASS: "+t.Name()+" ", "the child test binary ran no test:\n%s", output)

	return false
}
