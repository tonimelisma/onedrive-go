package synctest

import (
	"os"
	"strconv"
	"syscall"
	"testing"

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

// RunUnderUmask runs the calling test with the process umask set to umask
// and reports whether the caller is that run.
//
// The umask belongs to the whole process, so a test that sets it in a test
// binary running tests in parallel changes the mode of whatever the other
// tests create at that moment. The first call therefore runs the test again
// in a child test binary (RunInChild) with the umask in its environment,
// fails the test when that run fails or ran no test, and returns false: the
// caller returns at once. In the child the call sets the umask and returns
// true, and the test does its work there. Only a top-level test may call it,
// before anything else it does.
func RunUnderUmask(t *testing.T, umask os.FileMode) bool {
	t.Helper()

	octal := strconv.FormatUint(uint64(umask), 8)
	if InChild(t) {
		require.Equal(t, octal, os.Getenv(umaskChildEnv), "the child test binary was started for another umask")
		syscall.Umask(int(umask))

		return true
	}

	RequireChildPassed(t, runInChild(t, umaskChildEnv+"="+octal))

	return false
}
